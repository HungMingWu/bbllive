package rtmp

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	DEFAULT_POOL_SIZE = 4096
)

var pool = NewMediaFramePool(DEFAULT_POOL_SIZE)

func NewMediaFrame() *MediaFrame {
	return pool.New()
}

type MediaFrame struct {
	Idx            int
	Timestamp      uint32
	Type           byte //8 audio,9 video
	VideoFrameType byte //4bit
	VideoCodecID   byte //4bit
	AudioFormat    byte //4bit
	SamplingRate   byte //2bit
	SampleLength   byte //1bit
	AudioType      byte //1bit
	Payload        *bytes.Buffer
	StreamId       uint32
	count          int32
	p              *MediaFramePool
}

func (p *MediaFrame) IFrame() bool {
	return p.VideoFrameType == 1 || p.VideoFrameType == 4
}

func (p *MediaFrame) String() string {
	if p == nil {
		return "<nil>"
	}
	if p.Type == RTMP_MSG_AUDIO {
		return fmt.Sprintf("%v Audio Frame Timestamp/%v Type/%v AudioFromat/%v SampleRate/%v SampleLength/%v AudioType/%v Payload/%v StreamId/%v", p.Idx, float64(p.Timestamp)/1000.0, p.Type, audioformat[p.AudioFormat], samplerate[p.SamplingRate], samplelength[p.SampleLength], audiotype[p.AudioType], p.Payload.Len(), p.StreamId)
	} else if p.Type == RTMP_MSG_VIDEO {
		return fmt.Sprintf("%v Video Frame Timestamp/%v Type/%v VideoFrameType/%v VideoCodecID/%v Payload/%v StreamId/%v", p.Idx, float64(p.Timestamp)/1000.0, p.Type, videoframetype[p.VideoFrameType], videocodec[p.VideoCodecID], p.Payload.Len(), p.StreamId)
	}
	return fmt.Sprintf("%v Frame Timestamp/%v Type/%v Payload/%v StreamId/%v", p.Idx, p.Timestamp, p.Type, p.Payload.Len(), p.StreamId)
}

func (o *MediaFrame) Bytes() []byte {
	return o.Payload.Bytes()
}

func (o *MediaFrame) WriteTo(w io.Writer) (int, error) {
	return w.Write(o.Payload.Bytes())
}

type MediaFramePool struct {
	pool chan *MediaFrame
}

func NewMediaFramePool(size int) *MediaFramePool {
	return &MediaFramePool{pool: make(chan *MediaFrame, size)}
}

func (p *MediaFramePool) New() *MediaFrame {
	var x *MediaFrame
	x = &MediaFrame{count: 1, p: p, Payload: bytes.NewBuffer(nil)}
	return x
}

type NetStream interface {
	NsID() int
	Name() string
	String() string
	Notify(idx *int) error
	Close() error
	StreamObject() *StreamObject
}
type MediaGop struct {
	idx    int
	frames []*MediaFrame
	videoConfig *MediaFrame
	audioConfig *MediaFrame
	metaConfig  *MediaFrame
}

func (o *MediaGop) Release() {
	o.frames = o.frames[0:0]
}

func (o *MediaGop) Len() int {
	return len(o.frames)
}

type StreamObject struct {
	name     string
	duration uint32
	list     []int
	cache    map[int]*MediaGop
	subs               []NetStream
	subch              chan NetStream
	sublock            sync.RWMutex
	notify             chan *int
	lock               sync.RWMutex
	idx                int
	gidx               int
	csize              int
	metaData           *MediaFrame
	firstVideoKeyFrame *MediaFrame
	firstAudioKeyFrame *MediaFrame
	gop      *MediaGop
	streamid uint32
}

func new_streamObject(sid string, timeout time.Duration, record bool, csize int) (obj *StreamObject, err error) {
	obj = &StreamObject{
		name:   sid,
		list:   []int{},
		cache:  make(map[int]*MediaGop, csize),
		subs:   []NetStream{},
		notify: make(chan *int, csize*100),
		csize:  csize,
	}
	addObject(obj)
	go obj.loop(timeout)
	return obj, nil
}

func (m *StreamObject) Attach(c NetStream) {
	m.sublock.Lock()
	m.subs = append(m.subs, c)
	m.sublock.Unlock()
}
func (m *StreamObject) ReadGop(idx *int) *MediaGop {
	m.lock.RLock()
	if s, found := m.cache[*idx]; found {
		m.lock.RUnlock()
		return s
	}
	m.lock.RUnlock()
	log.Warn("Gop", m.name, *idx, "Not Found")
	return nil
}

func (m *StreamObject) WriteFrame(s *MediaFrame) (err error) {
	m.lock.Lock()
	if m.idx >= 0xffffffffffffff {
		m.idx = 0
	}
	s.Idx = m.idx
	m.idx += 1
	m.duration = s.Timestamp
	if s.Type == RTMP_MSG_VIDEO && s.IFrame() && m.firstVideoKeyFrame == nil {
		log.Info(">>>>", s)
		m.firstVideoKeyFrame = s
		m.streamid = s.StreamId
		m.lock.Unlock()
		return
	}
	if s.Type == RTMP_MSG_AUDIO && m.firstAudioKeyFrame == nil {
		log.Info(">>>>", s)
		m.firstAudioKeyFrame = s
		m.lock.Unlock()
		return
	}
	if s.Type == RTMP_MSG_AMF_META && m.metaData == nil {
		log.Info(">>>>", s)
		m.metaData = s
		m.lock.Unlock()
		return
	}
	if m.gop == nil {
		m.gop = &MediaGop{0, make([]*MediaFrame, 0), m.firstVideoKeyFrame, m.firstAudioKeyFrame, m.metaData}
	}

	if len(m.list) >= m.csize {
		idx := m.list[0]
		if s, found := m.cache[idx]; found {
			s.Release()
			delete(m.cache, idx)
		}
		m.list = m.list[1:]
	}
	if s.IFrame() && m.gop.Len() > 0 {
		gop := m.gop
		m.list = append(m.list, gop.idx)
		m.cache[gop.idx] = gop
		log.Info("Gop", m.name, gop.idx, gop.Len(), len(m.list))
		m.gop = &MediaGop{gop.idx + 1, []*MediaFrame{s}, m.firstVideoKeyFrame, m.firstAudioKeyFrame, m.metaData}
		m.lock.Unlock()
		m.notify <- &gop.idx
		return
	}
	m.gop.frames = append(m.gop.frames, s)
	m.lock.Unlock()
	return
}

func (m *StreamObject) Close() {
	removeObject(m.name)
	close(m.notify)
}
func (m *StreamObject) loop(timeout time.Duration) {
	log.Info(m.name, "stream object is runing")
	defer log.Info(m.name, "stream object is stopped")
	var (
		opened bool
		idx    *int
		w      NetStream
		err    error
		nsubs  = []NetStream{}
		subs   = []NetStream{}
	)
	defer m.clear()
	for {
		select {
		case idx, opened = <-m.notify:
			if !opened {
				return
			}
			m.sublock.Lock()
			nsubs = nsubs[0:0]
			subs = m.subs[:]
			m.sublock.Unlock()
			log.Info("players", m.name, len(subs))
			for _, w = range subs {
				if err = w.Notify(idx); err != nil {
					log.Error(w, err)
					w.Close()
				} else {
					nsubs = append(nsubs, w)
				}
			}
			m.sublock.Lock()
			m.subs = nsubs[:]
			m.sublock.Unlock()
		case <-time.After(timeout):
			m.Close()
		}
	}
}

func (m *StreamObject) clear() {
	m.sublock.Lock()
	for _, w := range m.subs {
		w.Close()
	}
	m.subs = m.subs[0:0]
	m.sublock.Unlock()
}
