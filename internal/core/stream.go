package core

import (
	"sync"

	"github.com/aler9/gortsplib"
)

type streamNonRTSPReadersMap struct {
	mutex sync.RWMutex
	ma    map[reader]struct{}
}

func newStreamNonRTSPReadersMap() *streamNonRTSPReadersMap {
	return &streamNonRTSPReadersMap{
		ma: make(map[reader]struct{}),
	}
}

func (m *streamNonRTSPReadersMap) close() {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.ma = nil
}

func (m *streamNonRTSPReadersMap) add(r reader) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	m.ma[r] = struct{}{}
}

func (m *streamNonRTSPReadersMap) remove(r reader) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	delete(m.ma, r)
}

func (m *streamNonRTSPReadersMap) writeData(data data) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	for c := range m.ma {
		c.onReaderData(data)
	}
}

func (m *streamNonRTSPReadersMap) hasReaders() bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return len(m.ma) > 0
}

type stream struct {
	nonRTSPReaders *streamNonRTSPReadersMap
	rtspStream     *gortsplib.ServerStream
	streamTracks   []streamTrack
}

func newStream(tracks gortsplib.Tracks, generateRTPPackets bool) (*stream, error) {
	s := &stream{
		nonRTSPReaders: newStreamNonRTSPReadersMap(),
		rtspStream:     gortsplib.NewServerStream(tracks),
	}

	s.streamTracks = make([]streamTrack, len(s.rtspStream.Tracks()))

	for i, track := range s.rtspStream.Tracks() {
		var err error
		s.streamTracks[i], err = newStreamTrack(track, generateRTPPackets)
		if err != nil {
			return nil, err
		}
	}

	return s, nil
}

func (s *stream) close() {
	s.nonRTSPReaders.close()
	s.rtspStream.Close()
}

func (s *stream) tracks() gortsplib.Tracks {
	return s.rtspStream.Tracks()
}

func (s *stream) readerAdd(r reader) {
	if _, ok := r.(pathRTSPSession); !ok {
		s.nonRTSPReaders.add(r)
	}
}

func (s *stream) readerRemove(r reader) {
	if _, ok := r.(pathRTSPSession); !ok {
		s.nonRTSPReaders.remove(r)
	}
}

func (s *stream) writeData(data data) {
	s.streamTracks[data.getTrackID()].onData(data, s.nonRTSPReaders.hasReaders())

	// forward RTP packets to RTSP readers
	for _, pkt := range data.getRTPPackets() {
		s.rtspStream.WritePacketRTP(data.getTrackID(), pkt, data.getPTSEqualsDTS())
	}

	// forward data to non-RTSP readers
	s.nonRTSPReaders.writeData(data)
}
