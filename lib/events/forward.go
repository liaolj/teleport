package events

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/gravitational/teleport/lib/session"

	"github.com/gravitational/trace"
)

// ForwarderConfig forwards session log events
// to the auth server, and writes the session playback to disk
type ForwarderConfig struct {
	// SessionID is a session id to write
	SessionID session.ID
	// ServerID is a serverID data directory
	ServerID string
	// DataDir is a data directory
	DataDir string
	// RecordSessions is a sessions recording setting
	RecordSessions bool
	// Namespace is a namespace of the session
	Namespace string
	// ForwardTo is the audit log to forward non-print events to
	ForwardTo IAuditLog
}

// CheckAndSetDefaults checks and sets default values
func (s *ForwarderConfig) CheckAndSetDefaults() error {
	if s.ForwardTo == nil {
		return trace.BadParameter("missing parameter bucket")
	}
	if s.DataDir == "" {
		return trace.BadParameter("missing data dir")
	}
	return nil
}

// NewForwarder returns a new instance of session forwarder
func NewForwarder(cfg ForwarderConfig) (*Forwarder, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	diskLogger, err := NewDiskSessionLogger(DiskSessionLoggerConfig{
		SessionID:      cfg.SessionID,
		DataDir:        cfg.DataDir,
		RecordSessions: cfg.RecordSessions,
		Namespace:      cfg.Namespace,
		ServerID:       cfg.ServerID,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &Forwarder{
		ForwarderConfig: cfg,
		sessionLogger:   diskLogger,
	}, nil
}

// ForwarderConfig forwards session log events
// to the auth server, and writes the session playback to disk
type Forwarder struct {
	ForwarderConfig
	sessionLogger *DiskSessionLogger
	lastChunk     *SessionChunk
	eventIndex    int64
	sync.Mutex
	isClosed bool
}

// Closer releases connection and resources associated with log if any
func (l *Forwarder) Close() error {
	l.Lock()
	defer l.Unlock()
	if l.isClosed {
		return nil
	}
	l.isClosed = true
	return l.sessionLogger.Finalize()
}

// EmitAuditEvent emits audit event
func (l *Forwarder) EmitAuditEvent(eventType string, fields EventFields) error {
	data, err := json.Marshal(fields)
	if err != nil {
		return trace.Wrap(err)
	}
	chunks := []*SessionChunk{
		{
			EventType: eventType,
			Data:      data,
			Time:      time.Now().UTC().UnixNano(),
		},
	}
	return l.PostSessionSlice(SessionSlice{
		Namespace: l.Namespace,
		SessionID: string(l.SessionID),
		Version:   V3,
		Chunks:    chunks,
	})
}

// PostSessionSlice sends chunks of recorded session to the event log
func (l *Forwarder) PostSessionSlice(slice SessionSlice) error {
	// setup slice sets slice verison, properly numerates
	// all chunks and
	chunksWithoutPrintEvents, err := l.setupSlice(&slice)
	if err != nil {
		return trace.Wrap(err)
	}

	// log all events and session recording locally
	err = l.sessionLogger.PostSessionSlice(slice)
	if err != nil {
		return trace.Wrap(err)
	}
	// no chunks to post (all chunks are print events)
	if len(chunksWithoutPrintEvents) == 0 {
		return nil
	}
	slice.Chunks = chunksWithoutPrintEvents
	slice.Version = V3
	err = l.ForwardTo.PostSessionSlice(slice)
	return err
}

func (l *Forwarder) setupSlice(slice *SessionSlice) ([]*SessionChunk, error) {
	l.Lock()
	defer l.Unlock()

	if l.isClosed {
		return nil, trace.BadParameter("write on closed forwarder")
	}

	// setup chunk indexes
	var chunks []*SessionChunk
	for _, chunk := range slice.Chunks {
		chunk.EventIndex = l.eventIndex
		l.eventIndex += 1
		switch chunk.EventType {
		case "":
			return nil, trace.BadParameter("missing event type")
		case SessionPrintEvent:
			// filter out chunks with session print events,
			// as this logger forwards only audit events to the auth server
			if l.lastChunk != nil {
				chunk.Offset = l.lastChunk.Offset + int64(len(l.lastChunk.Data))
				chunk.Delay = diff(time.Unix(0, l.lastChunk.Time), time.Unix(0, chunk.Time)) + l.lastChunk.Delay
				chunk.ChunkIndex = l.lastChunk.ChunkIndex + 1
			}
			l.lastChunk = chunk
		default:
			chunks = append(chunks, chunk)
		}
	}

	return chunks, nil
}

// UploadSessionRecording uploads session recording to the audit server
func (l *Forwarder) UploadSessionRecording(r SessionRecording) error {
	return l.ForwardTo.UploadSessionRecording(r)
}

// GetSessionChunk returns a reader which can be used to read a byte stream
// of a recorded session starting from 'offsetBytes' (pass 0 to start from the
// beginning) up to maxBytes bytes.
//
// If maxBytes > MaxChunkBytes, it gets rounded down to MaxChunkBytes
func (l *Forwarder) GetSessionChunk(namespace string, sid session.ID, offsetBytes, maxBytes int) ([]byte, error) {
	return l.ForwardTo.GetSessionChunk(namespace, sid, offsetBytes, maxBytes)
}

// Returns all events that happen during a session sorted by time
// (oldest first).
//
// after tells to use only return events after a specified cursor Id
//
// This function is usually used in conjunction with GetSessionReader to
// replay recorded session streams.
func (l *Forwarder) GetSessionEvents(namespace string, sid session.ID, after int, includePrintEvents bool) ([]EventFields, error) {
	return l.ForwardTo.GetSessionEvents(namespace, sid, after, includePrintEvents)
}

// SearchEvents is a flexible way to find  The format of a query string
// depends on the implementing backend. A recommended format is urlencoded
// (good enough for Lucene/Solr)
//
// Pagination is also defined via backend-specific query format.
//
// The only mandatory requirement is a date range (UTC). Results must always
// show up sorted by date (newest first)
func (l *Forwarder) SearchEvents(fromUTC, toUTC time.Time, query string, limit int) ([]EventFields, error) {
	return l.ForwardTo.SearchEvents(fromUTC, toUTC, query, limit)
}

// SearchSessionEvents returns session related events only. This is used to
// find completed session.
func (l *Forwarder) SearchSessionEvents(fromUTC time.Time, toUTC time.Time, limit int) ([]EventFields, error) {
	return l.ForwardTo.SearchSessionEvents(fromUTC, toUTC, limit)
}

// WaitForDelivery waits for resources to be released and outstanding requests to
// complete after calling Close method
func (l *Forwarder) WaitForDelivery(ctx context.Context) error {
	return l.ForwardTo.WaitForDelivery(ctx)
}
