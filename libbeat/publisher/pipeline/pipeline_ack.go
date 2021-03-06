package pipeline

import (
	"errors"

	"github.com/elastic/beats/libbeat/publisher/beat"
)

type ackBuilder interface {
	createPipelineACKer(canDrop bool, sema *sema) acker
	createCountACKer(canDrop bool, sema *sema, fn func(int)) acker
	createEventACKer(canDrop bool, sema *sema, fn func([]beat.Event)) acker
}

type pipelineEmptyACK struct {
	pipeline *Pipeline
}

func (b *pipelineEmptyACK) createPipelineACKer(canDrop bool, sema *sema) acker {
	return nilACKer
}

func (b *pipelineEmptyACK) createCountACKer(canDrop bool, sema *sema, fn func(int)) acker {
	return buildClientCountACK(b.pipeline, canDrop, sema, func(guard *clientACKer) func(int, int) {
		return func(total, acked int) {
			if guard.Active() {
				fn(total)
			}
		}
	})
}

func (b *pipelineEmptyACK) createEventACKer(
	canDrop bool,
	sema *sema,
	fn func([]beat.Event),
) acker {
	return buildClientEventACK(b.pipeline, canDrop, sema, func(guard *clientACKer) func([]beat.Event, int) {
		return func(events []beat.Event, acked int) {
			if guard.Active() {
				fn(events)
			}
		}
	})
}

type pipelineCountACK struct {
	pipeline *Pipeline
	cb       func(int, int)
}

func (b *pipelineCountACK) createPipelineACKer(canDrop bool, sema *sema) acker {
	return makeCountACK(b.pipeline, canDrop, sema, b.cb)
}

func (b *pipelineCountACK) createCountACKer(canDrop bool, sema *sema, fn func(int)) acker {
	return buildClientCountACK(b.pipeline, canDrop, sema, func(guard *clientACKer) func(int, int) {
		return func(total, acked int) {
			b.cb(total, acked)
			if guard.Active() {
				fn(total)
			}
		}
	})
}

func (b *pipelineCountACK) createEventACKer(
	canDrop bool,
	sema *sema,
	fn func([]beat.Event),
) acker {
	return buildClientEventACK(b.pipeline, canDrop, sema, func(guard *clientACKer) func([]beat.Event, int) {
		return func(events []beat.Event, acked int) {
			b.cb(len(events), acked)
			if guard.Active() {
				fn(events)
			}
		}
	})
}

type pipelineEventsACK struct {
	pipeline *Pipeline
	cb       func([]beat.Event, int)
}

func (b *pipelineEventsACK) createPipelineACKer(canDrop bool, sema *sema) acker {
	return newEventACK(b.pipeline, canDrop, sema, b.cb)
}

func (b *pipelineEventsACK) createCountACKer(canDrop bool, sema *sema, fn func(int)) acker {
	return buildClientEventACK(b.pipeline, canDrop, sema, func(guard *clientACKer) func([]beat.Event, int) {
		return func(events []beat.Event, acked int) {
			b.cb(events, acked)
			if guard.Active() {
				fn(len(events))
			}
		}
	})
}

func (b *pipelineEventsACK) createEventACKer(canDrop bool, sema *sema, fn func([]beat.Event)) acker {
	return buildClientEventACK(b.pipeline, canDrop, sema, func(guard *clientACKer) func([]beat.Event, int) {
		return func(events []beat.Event, acked int) {
			b.cb(events, acked)
			if guard.Active() {
				fn(events)
			}
		}
	})
}

// pipelineEventCB internally handles active ACKs in the pipeline.
// It receives ACK events from the broker and the individual clients.
// Once the broker returns an ACK to the pipelineEventCB, the worker loop will collect
// events from all clients having published events in the last batch of events
// being ACKed.
// the PipelineACKHandler will be notified, once all events being ACKed
// (including dropped events) have been collected. Only one ACK-event is handled
// at a time. The pipeline global and clients ACK handler will be blocked for the time
// an ACK event is being processed.
type pipelineEventCB struct {
	done chan struct{}

	acks chan int

	events        chan eventsMsg
	droppedEvents chan eventsMsg

	mode    pipelineACKMode
	handler beat.PipelineACKHandler
}

type eventsMsg struct {
	events       []beat.Event
	total, acked int
	sig          chan struct{}
}

type pipelineACKMode uint8

const (
	noACKMode pipelineACKMode = iota
	countACKMode
	eventsACKMode
	lastEventsACKMode
)

func newPipelineEventCB(handler beat.PipelineACKHandler) (*pipelineEventCB, error) {
	mode := noACKMode
	if handler.ACKCount != nil {
		mode = countACKMode
	}
	if handler.ACKEvents != nil {
		if mode != noACKMode {
			return nil, errors.New("only one callback can be set")
		}
		mode = eventsACKMode
	}
	if handler.ACKLastEvents != nil {
		if mode != noACKMode {
			return nil, errors.New("only one callback can be set")
		}
		mode = lastEventsACKMode
	}

	// yay, no work
	if mode == noACKMode {
		return nil, nil
	}

	cb := &pipelineEventCB{
		acks:          make(chan int),
		mode:          mode,
		handler:       handler,
		events:        make(chan eventsMsg),
		droppedEvents: make(chan eventsMsg),
	}
	go cb.worker()
	return cb, nil
}

func (p *pipelineEventCB) close() {
	close(p.done)
}

// reportEvents sends a batch of ACKed events to the ACKer.
// The events array contains send and dropped events. The `acked` counters
// indicates the total number of events acked by the pipeline.
// That is, the number of dropped events is given by `len(events) - acked`.
// A client can report events with acked=0, iff the client has no waiting events
// in the pipeline (ACK ordering requirements)
//
// Note: the call blocks, until the ACK handler has collected all active events
//       from all clients. This ensure an ACK event being fully 'captured'
//       by the pipeline, before receiving/processing another ACK event.
//       In the meantime the broker has the chance of batching-up more ACK events,
//       such that only one ACK event is being reported to the pipeline handler
func (p *pipelineEventCB) onEvents(events []beat.Event, acked int) {
	ch := p.events
	if acked == 0 {
		ch = p.droppedEvents
	}

	msg := eventsMsg{
		events: events,
		total:  len(events),
		acked:  acked,
		sig:    make(chan struct{}),
	}

	// send message to worker and wait for completion signal
	ch <- msg
	<-msg.sig
}

func (p *pipelineEventCB) onCounts(total, acked int) {
	ch := p.events
	if acked == 0 {
		ch = p.droppedEvents
	}

	msg := eventsMsg{
		total: total,
		acked: acked,
		sig:   make(chan struct{}),
	}

	ch <- msg
	<-msg.sig
}

// Starts a new ACKed event.
func (p *pipelineEventCB) reportBrokerACK(acked int) {
	p.acks <- acked
}

func (p *pipelineEventCB) worker() {
	defer close(p.acks)
	defer close(p.events)
	defer close(p.droppedEvents)

	for {
		select {
		case count := <-p.acks:
			exit := p.collect(count)
			if exit {
				return
			}

			// short circuite dropped events, but have client block until all events
			// have been processed by pipeline ack handler
		case msg := <-p.droppedEvents:
			p.reportEvents(msg.events, msg.total)
			close(msg.sig)

		case <-p.done:
			return
		}
	}
}

func (p *pipelineEventCB) collect(count int) (exit bool) {
	var (
		signalers []chan struct{}
		events    []beat.Event
		acked     int
		total     int
	)

	for acked < count {
		var msg eventsMsg
		select {
		case msg = <-p.events:
		case msg = <-p.droppedEvents:
		case <-p.done:
			exit = true
			return
		}

		signalers = append(signalers, msg.sig)
		total += msg.total
		acked += msg.acked

		if count-acked < 0 {
			panic("ack count mismatch")
		}

		switch p.mode {
		case eventsACKMode:
			events = append(events, msg.events...)

		case lastEventsACKMode:
			if L := len(msg.events); L > 0 {
				events = append(events, msg.events[L-1])
			}
		}
	}

	// signal clients we processed all active ACKs, as reported by broker
	for _, sig := range signalers {
		close(sig)
	}
	p.reportEvents(events, total)
	return
}

func (p *pipelineEventCB) reportEvents(events []beat.Event, total int) {
	// report ACK back to the beat
	switch p.mode {
	case countACKMode:
		p.handler.ACKCount(total)
	case eventsACKMode:
		p.handler.ACKEvents(events)
	case lastEventsACKMode:
		p.handler.ACKLastEvents(events)
	}
}
