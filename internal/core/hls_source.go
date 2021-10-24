package core

import (
	"context"
	"sync"
	"time"

	"github.com/aler9/gortsplib"

	"github.com/aler9/rtsp-simple-server/internal/hls"
	"github.com/aler9/rtsp-simple-server/internal/conf"
	"github.com/aler9/rtsp-simple-server/internal/logger"
	"github.com/aler9/rtsp-simple-server/internal/rtcpsenderset"
)

type hlsSourceParent interface {
	Log(logger.Level, string, ...interface{})
	OnSourceStaticSetReady(req pathSourceStaticSetReadyReq) pathSourceStaticSetReadyRes
	OnSourceStaticSetNotReady(req pathSourceStaticSetNotReadyReq)
}

type hlsSource struct {
	ur     string
	wg     *sync.WaitGroup
	parent hlsSourceParent

	ctx       context.Context
	ctxCancel func()
}

func newHLSSource(
	parentCtx context.Context,
	ur string,
	retryPause conf.StringDuration,
	wg *sync.WaitGroup,
	parent hlsSourceParent) *hlsSource {
	ctx, ctxCancel := context.WithCancel(parentCtx)

	s := &hlsSource{
		ur:        ur,
		wg:        wg,
		parent:    parent,
		ctx:       ctx,
		ctxCancel: ctxCancel,
	}

	s.Log(logger.Info, "started")

	s.wg.Add(1)
	go s.run(retryPause)

	return s
}

func (s *hlsSource) Close() {
	s.Log(logger.Info, "stopped")
	s.ctxCancel()
}

func (s *hlsSource) Log(level logger.Level, format string, args ...interface{}) {
	s.parent.Log(level, "[hls source] "+format, args...)
}

func (s *hlsSource) run(retryPause conf.StringDuration) {
	defer s.wg.Done()

outer:
	for {
		ok := s.runInner()
		if !ok {
			break outer
		}

		select {
		case <-time.After(time.Duration(retryPause)):
		case <-s.ctx.Done():
			break outer
		}
	}

	s.ctxCancel()
}

func (s *hlsSource) runInner() bool {
	var stream *stream
	var rtcpSenders *rtcpsenderset.RTCPSenderSet
	var videoTrackID int
	var audioTrackID int

	defer func() {
		if stream != nil {
			s.parent.OnSourceStaticSetNotReady(pathSourceStaticSetNotReadyReq{Source: s})
			rtcpSenders.Close()
		}
	}()

	onTracks := func(videoTrack *gortsplib.Track, audioTrack *gortsplib.Track) error {
		var tracks gortsplib.Tracks

		if videoTrack != nil {
			videoTrackID = len(tracks)
			tracks = append(tracks, videoTrack)
		}

		if audioTrack != nil {
			audioTrackID = len(tracks)
			tracks = append(tracks, audioTrack)
		}

		res := s.parent.OnSourceStaticSetReady(pathSourceStaticSetReadyReq{
			Source: s,
			Tracks: tracks,
		})
		if res.Err != nil {
			return res.Err
		}

		s.Log(logger.Info, "ready")

		stream = res.Stream
		rtcpSenders = rtcpsenderset.New(tracks, stream.onFrame)

		return nil
	}

	onFrame := func(isVideo bool, payload []byte) {
		var trackID int
		if isVideo {
			trackID = videoTrackID
		} else {
			trackID = audioTrackID
		}

		if stream != nil {
			rtcpSenders.OnFrame(trackID, gortsplib.StreamTypeRTP, payload)
			stream.onFrame(trackID, gortsplib.StreamTypeRTP, payload)
		}
	}

	c := hls.NewClient(
		s.ur,
		onTracks,
		onFrame,
		s,
	)

	select {
	case err := <-c.Wait():
		s.Log(logger.Info, "ERR: %v", err)
		return true

	case <-s.ctx.Done():
		c.Close()
		<-c.Wait()
		return false
	}
}

// OnSourceAPIDescribe implements source.
func (*hlsSource) OnSourceAPIDescribe() interface{} {
	return struct {
		Type string `json:"type"`
	}{"hlsSource"}
}
