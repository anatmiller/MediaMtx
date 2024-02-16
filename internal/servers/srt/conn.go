package srt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v4/pkg/description"
	mcmpegts "github.com/bluenviron/mediacommon/pkg/formats/mpegts"
	srt "github.com/datarhei/gosrt"
	"github.com/google/uuid"

	"github.com/bluenviron/mediamtx/internal/asyncwriter"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/hooks"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/mpegts"
	"github.com/bluenviron/mediamtx/internal/stream"
)

const (
	pauseAfterAuthError = 2 * time.Second
)

func srtCheckPassphrase(connReq srt.ConnRequest, passphrase string) error {
	if passphrase == "" {
		return nil
	}

	if !connReq.IsEncrypted() {
		return fmt.Errorf("connection is encrypted, but not passphrase is defined in configuration")
	}

	err := connReq.SetPassphrase(passphrase)
	if err != nil {
		return fmt.Errorf("invalid passphrase")
	}

	return nil
}

type connState int

const (
	connStateRead connState = iota + 1
	connStatePublish
)

type conn struct {
	parentCtx           context.Context
	rtspAddress         string
	readTimeout         conf.StringDuration
	writeTimeout        conf.StringDuration
	writeQueueSize      int
	udpMaxPayloadSize   int
	connReq             srt.ConnRequest
	runOnConnect        string
	runOnConnectRestart bool
	runOnDisconnect     string
	wg                  *sync.WaitGroup
	externalCmdPool     *externalcmd.Pool
	pathManager         defs.PathManager
	parent              *Server

	ctx       context.Context
	ctxCancel func()
	created   time.Time
	uuid      uuid.UUID
	mutex     sync.RWMutex
	state     connState
	pathName  string
	query     string
	sconn     srt.Conn

	chNew     chan srtNewConnReq
	chSetConn chan srt.Conn
}

func (c *conn) initialize() {
	c.ctx, c.ctxCancel = context.WithCancel(c.parentCtx)

	c.created = time.Now()
	c.uuid = uuid.New()
	c.chNew = make(chan srtNewConnReq)
	c.chSetConn = make(chan srt.Conn)

	c.Log(logger.Info, "opened")

	c.wg.Add(1)
	go c.run()
}

func (c *conn) Close() {
	c.ctxCancel()
}

// Log implements logger.Writer.
func (c *conn) Log(level logger.Level, format string, args ...interface{}) {
	c.parent.Log(level, "[conn %v] "+format, append([]interface{}{c.connReq.RemoteAddr()}, args...)...)
}

func (c *conn) ip() net.IP {
	return c.connReq.RemoteAddr().(*net.UDPAddr).IP
}

func (c *conn) run() { //nolint:dupl
	defer c.wg.Done()

	onDisconnectHook := hooks.OnConnect(hooks.OnConnectParams{
		Logger:              c,
		ExternalCmdPool:     c.externalCmdPool,
		RunOnConnect:        c.runOnConnect,
		RunOnConnectRestart: c.runOnConnectRestart,
		RunOnDisconnect:     c.runOnDisconnect,
		RTSPAddress:         c.rtspAddress,
		Desc:                c.APIReaderDescribe(),
	})
	defer onDisconnectHook()

	err := c.runInner()

	c.ctxCancel()

	c.parent.closeConn(c)

	c.Log(logger.Info, "closed: %v", err)
}

func (c *conn) runInner() error {
	var req srtNewConnReq
	select {
	case req = <-c.chNew:
	case <-c.ctx.Done():
		return errors.New("terminated")
	}

	answerSent, err := c.runInner2(req)

	if !answerSent {
		req.res <- nil
	}

	return err
}

func (c *conn) runInner2(req srtNewConnReq) (bool, error) {
	var streamID streamID
	err := streamID.unmarshal(req.connReq.StreamId())
	if err != nil {
		return false, fmt.Errorf("invalid stream ID '%s': %w", req.connReq.StreamId(), err)
	}

	if streamID.mode == streamIDModePublish {
		return c.runPublish(req, &streamID)
	}
	return c.runRead(req, &streamID)
}

func (c *conn) runPublish(req srtNewConnReq, streamID *streamID) (bool, error) {
	path, err := c.pathManager.AddPublisher(defs.PathAddPublisherReq{
		Author: c,
		AccessRequest: defs.PathAccessRequest{
			Name:    streamID.path,
			IP:      c.ip(),
			Publish: true,
			User:    streamID.user,
			Pass:    streamID.pass,
			Proto:   defs.AuthProtocolSRT,
			ID:      &c.uuid,
			Query:   streamID.query,
		},
	})
	if err != nil {
		var terr defs.AuthenticationError
		if errors.As(err, &terr) {
			// wait some seconds to mitigate brute force attacks
			<-time.After(pauseAfterAuthError)
			return false, terr
		}
		return false, err
	}

	defer path.RemovePublisher(defs.PathRemovePublisherReq{Author: c})

	err = srtCheckPassphrase(req.connReq, path.SafeConf().SRTPublishPassphrase)
	if err != nil {
		return false, err
	}

	sconn, err := c.exchangeRequestWithConn(req)
	if err != nil {
		return true, err
	}

	c.mutex.Lock()
	c.state = connStatePublish
	c.pathName = streamID.path
	c.query = streamID.query
	c.sconn = sconn
	c.mutex.Unlock()

	readerErr := make(chan error)
	go func() {
		readerErr <- c.runPublishReader(sconn, path)
	}()

	select {
	case err := <-readerErr:
		sconn.Close()
		return true, err

	case <-c.ctx.Done():
		sconn.Close()
		<-readerErr
		return true, errors.New("terminated")
	}
}

func (c *conn) runPublishReader(sconn srt.Conn, path defs.Path) error {
	sconn.SetReadDeadline(time.Now().Add(time.Duration(c.readTimeout)))
	r, err := mcmpegts.NewReader(mcmpegts.NewBufferedReader(sconn))
	if err != nil {
		return err
	}

	decodeErrLogger := logger.NewLimitedLogger(c)

	r.OnDecodeError(func(err error) {
		decodeErrLogger.Log(logger.Warn, err.Error())
	})

	var stream *stream.Stream

	medias, err := mpegts.ToStream(r, &stream)
	if err != nil {
		return err
	}

	stream, err = path.StartPublisher(defs.PathStartPublisherReq{
		Author:             c,
		Desc:               &description.Session{Medias: medias},
		GenerateRTPPackets: true,
	})
	if err != nil {
		return err
	}

	for {
		err := r.Read()
		if err != nil {
			return err
		}
	}
}

func (c *conn) runRead(req srtNewConnReq, streamID *streamID) (bool, error) {
	path, stream, err := c.pathManager.AddReader(defs.PathAddReaderReq{
		Author: c,
		AccessRequest: defs.PathAccessRequest{
			Name:  streamID.path,
			IP:    c.ip(),
			User:  streamID.user,
			Pass:  streamID.pass,
			Proto: defs.AuthProtocolSRT,
			ID:    &c.uuid,
			Query: streamID.query,
		},
	})
	if err != nil {
		var terr defs.AuthenticationError
		if errors.As(err, &terr) {
			// wait some seconds to mitigate brute force attacks
			<-time.After(pauseAfterAuthError)
			return false, err
		}
		return false, err
	}

	defer path.RemoveReader(defs.PathRemoveReaderReq{Author: c})

	err = srtCheckPassphrase(req.connReq, path.SafeConf().SRTReadPassphrase)
	if err != nil {
		return false, err
	}

	sconn, err := c.exchangeRequestWithConn(req)
	if err != nil {
		return true, err
	}
	defer sconn.Close()

	c.mutex.Lock()
	c.state = connStateRead
	c.pathName = streamID.path
	c.query = streamID.query
	c.sconn = sconn
	c.mutex.Unlock()

	writer := asyncwriter.New(c.writeQueueSize, c)

	defer stream.RemoveReader(writer)

	bw := bufio.NewWriterSize(sconn, srtMaxPayloadSize(c.udpMaxPayloadSize))

	err = mpegts.FromStream(stream, writer, bw, sconn, time.Duration(c.writeTimeout))
	if err != nil {
		return true, err
	}

	c.Log(logger.Info, "is reading from path '%s', %s",
		path.Name(), defs.FormatsInfo(stream.FormatsForReader(writer)))

	onUnreadHook := hooks.OnRead(hooks.OnReadParams{
		Logger:          c,
		ExternalCmdPool: c.externalCmdPool,
		Conf:            path.SafeConf(),
		ExternalCmdEnv:  path.ExternalCmdEnv(),
		Reader:          c.APIReaderDescribe(),
		Query:           streamID.query,
	})
	defer onUnreadHook()

	// disable read deadline
	sconn.SetReadDeadline(time.Time{})

	writer.Start()

	select {
	case <-c.ctx.Done():
		writer.Stop()
		return true, fmt.Errorf("terminated")

	case err := <-writer.Error():
		return true, err
	}
}

func (c *conn) exchangeRequestWithConn(req srtNewConnReq) (srt.Conn, error) {
	req.res <- c

	select {
	case sconn := <-c.chSetConn:
		return sconn, nil

	case <-c.ctx.Done():
		return nil, errors.New("terminated")
	}
}

// new is called by srtListener through srtServer.
func (c *conn) new(req srtNewConnReq) *conn {
	select {
	case c.chNew <- req:
		return <-req.res

	case <-c.ctx.Done():
		return nil
	}
}

// setConn is called by srtListener .
func (c *conn) setConn(sconn srt.Conn) {
	select {
	case c.chSetConn <- sconn:
	case <-c.ctx.Done():
	}
}

// APIReaderDescribe implements reader.
func (c *conn) APIReaderDescribe() defs.APIPathSourceOrReader {
	return defs.APIPathSourceOrReader{
		Type: "srtConn",
		ID:   c.uuid.String(),
	}
}

// APISourceDescribe implements source.
func (c *conn) APISourceDescribe() defs.APIPathSourceOrReader {
	return c.APIReaderDescribe()
}

func (c *conn) apiItem() *defs.APISRTConn {
	c.mutex.RLock()
	defer c.mutex.RUnlock()

	var connMetrics defs.APISRTConnMetrics
	if c.sconn != nil {
		var s srt.Statistics
		c.sconn.Stats(&s)

		connMetrics.PacketsSent = s.Accumulated.PktSent
		connMetrics.PacketsReceived = s.Accumulated.PktRecv
		connMetrics.PacketsSentUnique = s.Accumulated.PktSentUnique
		connMetrics.PacketsReceivedUnique = s.Accumulated.PktRecvUnique
		connMetrics.PacketsSendLoss = s.Accumulated.PktSendLoss
		connMetrics.PacketsReceivedLoss = s.Accumulated.PktRecvLoss
		connMetrics.PacketsRetrans = s.Accumulated.PktRetrans
		connMetrics.PacketsReceivedRetrans = s.Accumulated.PktRecvRetrans
		connMetrics.PacketsSentACK = s.Accumulated.PktSentACK
		connMetrics.PacketsReceivedACK = s.Accumulated.PktRecvACK
		connMetrics.PacketsSentNAK = s.Accumulated.PktSentNAK
		connMetrics.PacketsReceivedNAK = s.Accumulated.PktRecvNAK
		connMetrics.PacketsSentKM = s.Accumulated.PktSentKM
		connMetrics.PacketsReceivedKM = s.Accumulated.PktRecvKM
		connMetrics.UsSndDuration = s.Accumulated.UsSndDuration
		connMetrics.PacketsReceivedBelated = s.Accumulated.PktRecvBelated
		connMetrics.PacketsSendDrop = s.Accumulated.PktSendDrop
		connMetrics.PacketsReceivedDrop = s.Accumulated.PktRecvDrop
		connMetrics.PacketsReceivedUndecrypt = s.Accumulated.PktRecvUndecrypt
		connMetrics.BytesSent = s.Accumulated.ByteSent
		connMetrics.BytesReceived = s.Accumulated.ByteRecv
		connMetrics.BytesSentUnique = s.Accumulated.ByteSentUnique
		connMetrics.BytesReceivedUnique = s.Accumulated.ByteRecvUnique
		connMetrics.BytesReceivedLoss = s.Accumulated.ByteRecvLoss
		connMetrics.BytesRetrans = s.Accumulated.ByteRetrans
		connMetrics.BytesReceivedRetrans = s.Accumulated.ByteRecvRetrans
		connMetrics.BytesReceivedBelated = s.Accumulated.ByteRecvBelated
		connMetrics.BytesSendDrop = s.Accumulated.ByteSendDrop
		connMetrics.BytesReceivedDrop = s.Accumulated.ByteRecvDrop
		connMetrics.BytesReceivedUndecrypt = s.Accumulated.ByteRecvUndecrypt
		connMetrics.UsPacketsSendPeriod = s.Instantaneous.UsPktSendPeriod
		connMetrics.PacketsFlowWindow = s.Instantaneous.PktFlowWindow
		connMetrics.PacketsFlightSize = s.Instantaneous.PktFlightSize
		connMetrics.MsRTT = s.Instantaneous.MsRTT
		connMetrics.MbpsSendRate = s.Instantaneous.MbpsSentRate
		connMetrics.MbpsReceiveRate = s.Instantaneous.MbpsRecvRate
		connMetrics.MbpsLinkCapacity = s.Instantaneous.MbpsLinkCapacity
		connMetrics.BytesAvailSendBuf = s.Instantaneous.ByteAvailSendBuf
		connMetrics.BytesAvailReceiveBuf = s.Instantaneous.ByteAvailRecvBuf
		connMetrics.MbpsMaxBW = s.Instantaneous.MbpsMaxBW
		connMetrics.ByteMSS = s.Instantaneous.ByteMSS
		connMetrics.PacketsSendBuf = s.Instantaneous.PktSendBuf
		connMetrics.BytesSendBuf = s.Instantaneous.ByteSendBuf
		connMetrics.MsSendBuf = s.Instantaneous.MsSendBuf
		connMetrics.MsSendTsbPdDelay = s.Instantaneous.MsSendTsbPdDelay
		connMetrics.PacketsReceiveBuf = s.Instantaneous.PktRecvBuf
		connMetrics.BytesReceiveBuf = s.Instantaneous.ByteRecvBuf
		connMetrics.MsReceiveBuf = s.Instantaneous.MsRecvBuf
		connMetrics.MsReceiveTsbPdDelay = s.Instantaneous.MsRecvTsbPdDelay
		connMetrics.PacketsReorderTolerance = s.Instantaneous.PktReorderTolerance
		connMetrics.PacketsReceivedAvgBelatedTime = s.Instantaneous.PktRecvAvgBelatedTime
		connMetrics.PacketsSendLossRate = s.Instantaneous.PktSendLossRate
		connMetrics.PacketsReceivedLossRate = s.Instantaneous.PktRecvLossRate
	}

	return &defs.APISRTConn{
		ID:         c.uuid,
		Created:    c.created,
		RemoteAddr: c.connReq.RemoteAddr().String(),
		State: func() defs.APISRTConnState {
			switch c.state {
			case connStateRead:
				return defs.APISRTConnStateRead

			case connStatePublish:
				return defs.APISRTConnStatePublish

			default:
				return defs.APISRTConnStateIdle
			}
		}(),
		Path:              c.pathName,
		Query:             c.query,
		APISRTConnMetrics: connMetrics,
	}
}
