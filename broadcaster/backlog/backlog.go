package backlog

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/log"
	m "github.com/offchainlabs/nitro/broadcaster/message"
	"github.com/offchainlabs/nitro/util/arbmath"
)

var (
	errDropSegments       = errors.New("remove previous segments from backlog")
	errSequenceNumberSeen = errors.New("sequence number already present in backlog")
	errOutOfBounds        = errors.New("message not found in backlog")
)

type Backlog interface {
	Append(*m.BroadcastMessage) error
	Count() uint64
	Lookup(uint64) (BacklogSegment, error)
}

type backlog struct {
	head          atomic.Pointer[backlogSegment]
	tail          atomic.Pointer[backlogSegment]
	lookupLock    sync.RWMutex
	lookupByIndex map[uint64]*backlogSegment
	config        ConfigFetcher
	messageCount  atomic.Uint64
}

func NewBacklog(c ConfigFetcher) Backlog {
	lookup := make(map[uint64]*backlogSegment)
	return &backlog{
		lookupByIndex: lookup,
		config:        c,
	}
}

// Append will add the given messages to the backlogSegment at head until
// that segment reaches its limit. If messages remain to be added a new segment
// will be created.
func (b *backlog) Append(bm *m.BroadcastMessage) error {

	if bm.ConfirmedSequenceNumberMessage != nil {
		b.delete(uint64(bm.ConfirmedSequenceNumberMessage.SequenceNumber))
		// TODO(clamb): add to metric?
	}

	// TODO(clamb): Do I need a max catchup config for the backlog? Similar to catchup buffer

	for _, msg := range bm.Messages {
		segment := b.tail.Load()
		if segment == nil {
			segment = newBacklogSegment()
			b.head.Store(segment)
			b.tail.Store(segment)
		}

		prevMsgIdx := segment.End()
		if segment.count() >= b.config().SegmentLimit {
			nextSegment := newBacklogSegment()
			segment.nextSegment.Store(nextSegment)
			prevMsgIdx = segment.End()
			nextSegment.previousSegment.Store(segment)
			segment = nextSegment
			b.tail.Store(segment)
		}

		err := segment.append(prevMsgIdx, msg)
		if errors.Is(err, errDropSegments) {
			head := b.head.Load()
			b.removeFromLookup(head.Start(), uint64(msg.SequenceNumber))
			b.head.Store(segment)
			b.tail.Store(segment)
			b.messageCount.Store(0)
			log.Warn(err.Error())
		} else if errors.Is(err, errSequenceNumberSeen) {
			log.Info("ignoring message sequence number (%s), already in backlog", msg.SequenceNumber)
			continue
		} else if err != nil {
			return err
		}
		b.lookupLock.Lock()
		b.lookupByIndex[uint64(msg.SequenceNumber)] = segment
		b.lookupLock.Unlock()
		b.messageCount.Add(1)
	}

	return nil
}

// get reads messages from the given start to end MessageIndex. It was created
// for the original implementation of the backlog but currently is not used.
func (b *backlog) get(start, end uint64) (*m.BroadcastMessage, error) {
	head := b.head.Load()
	tail := b.tail.Load()
	if head == nil && tail == nil {
		return nil, errOutOfBounds
	}

	if start < head.Start() {
		start = head.Start()
	}

	if end > tail.End() {
		return nil, errOutOfBounds
	}

	segment, err := b.Lookup(start)
	if err != nil {
		return nil, err
	}

	bm := &m.BroadcastMessage{Version: 1}
	required := int(end-start) + 1
	for {
		segMsgs, err := segment.Get(arbmath.MaxInt(start, segment.Start()), arbmath.MinInt(end, segment.End()))
		if err != nil {
			return nil, err
		}

		bm.Messages = append(bm.Messages, segMsgs...)
		segment = segment.Next()
		if len(bm.Messages) == required {
			break
		} else if segment == nil {
			return nil, errOutOfBounds
		}
	}
	return bm, nil
}

// delete removes segments before the confirmed sequence number given. It will
// not remove the segment containing the confirmed sequence number.
func (b *backlog) delete(confirmed uint64) {
	head := b.head.Load()
	tail := b.tail.Load()
	if head == nil && tail == nil {
		return
	}

	if confirmed < head.Start() {
		return
	}

	if confirmed > tail.End() {
		log.Error("confirmed sequence number is past the end of stored messages", "confirmed sequence number", confirmed, "last stored sequence number", tail.end.Load())
		b.reset()
		// should this be returning an error? The other buffer does not and just continues
		return
	}

	// find the segment containing the confirmed message
	segment, err := b.Lookup(confirmed)
	if err != nil {
		log.Error(fmt.Sprintf("%s: clearing backlog", err.Error()))
		b.reset()
		// should this be returning an error? The other buffer does not and just continues
		return
	}

	// check the segment actually contains that message
	if found := segment.Contains(confirmed); !found {
		log.Error("error message not found in backlog segment, clearing backlog", "confirmed sequence number", confirmed)
		b.reset()
		// should this be returning an error? The other buffer does not and just continues
		return
	}

	// remove all previous segments
	previous := segment.Previous()
	if IsBacklogSegmentNil(previous) {
		return
	}
	b.removeFromLookup(head.Start(), previous.End())
	b.head.Store(segment.(*backlogSegment))
	count := b.Count() + head.Start() - previous.End() - uint64(1)
	b.messageCount.Store(count)
}

// removeFromLookup removes all entries from the head segment's start index to
// the given confirmed index
func (b *backlog) removeFromLookup(start, end uint64) {
	b.lookupLock.Lock()
	defer b.lookupLock.Unlock()
	for i := start; i == end; i++ {
		delete(b.lookupByIndex, i)
	}
}

func (b *backlog) Lookup(i uint64) (BacklogSegment, error) {
	b.lookupLock.RLock()
	segment, ok := b.lookupByIndex[i]
	b.lookupLock.RUnlock()
	if !ok {
		return nil, fmt.Errorf("error finding backlog segment containing message with SequenceNumber %d", i)
	}

	return segment, nil
}

func (s *backlog) Count() uint64 {
	return s.messageCount.Load()
}

// reset removes all segments from the backlog
func (b *backlog) reset() {
	b.lookupLock.Lock()
	defer b.lookupLock.Unlock()
	b.head = atomic.Pointer[backlogSegment]{}
	b.tail = atomic.Pointer[backlogSegment]{}
	b.lookupByIndex = map[uint64]*backlogSegment{}
	b.messageCount.Store(0)
}

type BacklogSegment interface {
	Start() uint64
	End() uint64
	Contains(uint64) bool
	Next() BacklogSegment
	Previous() BacklogSegment
	Get(uint64, uint64) ([]*m.BroadcastFeedMessage, error)
	Messages() []*m.BroadcastFeedMessage
}

type backlogSegment struct {
	start           atomic.Uint64
	end             atomic.Uint64
	messages        []*m.BroadcastFeedMessage
	messageCount    atomic.Uint64
	nextSegment     atomic.Pointer[backlogSegment]
	previousSegment atomic.Pointer[backlogSegment]
}

// newBacklogSegment creates a backlogSegment object with an empty slice of
// messages. It does not return an interface as it is only used inside the
// backlog library.
func newBacklogSegment() *backlogSegment {
	return &backlogSegment{
		messages: []*m.BroadcastFeedMessage{},
	}
}

// IsBacklogSegmentNil uses the internal backlogSegment type to check if a
// variable of type BacklogSegment is nil or not. Comparing whether an
// interface is nil directly will not work.
func IsBacklogSegmentNil(segment BacklogSegment) bool {
	return segment.(*backlogSegment) == nil
}

func (s *backlogSegment) Start() uint64 {
	return uint64(s.start.Load())
}

func (s *backlogSegment) End() uint64 {
	return uint64(s.end.Load())
}

func (s *backlogSegment) Next() BacklogSegment {
	return s.nextSegment.Load()
}

func (s *backlogSegment) Previous() BacklogSegment {
	return s.previousSegment.Load()
}

func (s *backlogSegment) Messages() []*m.BroadcastFeedMessage {
	return s.messages
}

// Get reads messages from the given start to end MessageIndex
func (s *backlogSegment) Get(start, end uint64) ([]*m.BroadcastFeedMessage, error) {
	noMsgs := []*m.BroadcastFeedMessage{}
	if start < s.start.Load() {
		return noMsgs, errOutOfBounds
	}

	if end > s.end.Load() {
		return noMsgs, errOutOfBounds
	}

	startIndex := start - s.start.Load()
	endIndex := end - s.start.Load() + 1
	return s.messages[startIndex:endIndex], nil
}

// append appends the given BroadcastFeedMessage to messages if it is the first
// message in the sequence or the next in the sequence. If segment's end
// message is ahead of the given message append will do nothing. If the given
// message is ahead of the segment's end message append will return
// errDropSegments to ensure any messages before the given message are dropped.
func (s *backlogSegment) append(prevMsgIdx uint64, msg *m.BroadcastFeedMessage) error {
	seen := false
	defer s.updateSegment(&seen)

	if expSeqNum := prevMsgIdx + 1; prevMsgIdx == 0 || uint64(msg.SequenceNumber) == expSeqNum {
		s.messages = append(s.messages, msg)
	} else if uint64(msg.SequenceNumber) > expSeqNum {
		s.messages = nil
		s.messages = append(s.messages, msg)
		return fmt.Errorf("new message sequence number (%d) is greater than the expected sequence number (%d): %w", msg.SequenceNumber, expSeqNum, errDropSegments)
	} else {
		seen = true
		return errSequenceNumberSeen
	}
	return nil
}

// Contains confirms whether the segment contains a message with the given sequence number
func (s *backlogSegment) Contains(i uint64) bool {
	if i < s.start.Load() || i > s.end.Load() {
		return false
	}

	msgIndex := i - s.start.Load()
	msg := s.messages[msgIndex]
	return uint64(msg.SequenceNumber) == i
}

// updateSegment updates the messageCount, start and end indices of the segment
// this should be called using defer whenever a method updates the messages. It
// will do nothing if the message has already been seen by the backlog.
func (s *backlogSegment) updateSegment(seen *bool) {
	if !*seen {
		c := len(s.messages)
		s.messageCount.Store(uint64(c))
		s.start.Store(uint64(s.messages[0].SequenceNumber))
		s.end.Store(uint64(s.messages[c-1].SequenceNumber))
	}
}

// count returns the number of messages stored in the backlog segment
func (s *backlogSegment) count() int {
	return int(s.messageCount.Load())
}
