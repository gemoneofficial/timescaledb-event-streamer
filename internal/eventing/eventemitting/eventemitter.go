package eventemitting

import (
	"encoding/base64"
	"encoding/binary"
	"github.com/cenkalti/backoff/v4"
	"github.com/jackc/pglogrepl"
	"github.com/noctarius/timescaledb-event-streamer/internal/eventing/eventfiltering"
	"github.com/noctarius/timescaledb-event-streamer/internal/replication/context"
	"github.com/noctarius/timescaledb-event-streamer/internal/replication/transactional"
	"github.com/noctarius/timescaledb-event-streamer/spi/eventhandlers"
	"github.com/noctarius/timescaledb-event-streamer/spi/pgtypes"
	"github.com/noctarius/timescaledb-event-streamer/spi/schema"
	"github.com/noctarius/timescaledb-event-streamer/spi/sink"
	"github.com/noctarius/timescaledb-event-streamer/spi/systemcatalog"
	"time"
)

type EventEmitter struct {
	replicationContext *context.ReplicationContext
	transactionMonitor *transactional.TransactionMonitor
	filter             eventfiltering.EventFilter
	sink               sink.Sink
	sinkContext        *sinkContextImpl
	backOff            backoff.BackOff
}

func NewEventEmitter(
	replicationContext *context.ReplicationContext, transactionMonitor *transactional.TransactionMonitor,
	sink sink.Sink, filter eventfiltering.EventFilter) *EventEmitter {

	return &EventEmitter{
		replicationContext: replicationContext,
		transactionMonitor: transactionMonitor,
		filter:             filter,
		sink:               sink,
		sinkContext:        newSinkContextImpl(),
		backOff:            backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 8),
	}
}

func (ee *EventEmitter) Start() error {
	if encodedSinkContextState, present := ee.replicationContext.EncodedState("SinkContextState"); present {
		return ee.sinkContext.UnmarshalBinary(encodedSinkContextState)
	}
	ee.replicationContext.RegisterStateEncoder("SinkContextState", ee.sinkContext.MarshalBinary)
	return nil
}

func (ee *EventEmitter) Stop() error {
	return nil
}

func (ee *EventEmitter) NewEventHandler() eventhandlers.BaseReplicationEventHandler {
	return &eventEmitterEventHandler{
		eventEmitter: ee,
	}
}

func (ee *EventEmitter) envelopeSchema(hypertable *systemcatalog.Hypertable) schema.Struct {
	schemaTopicName := ee.replicationContext.HypertableEnvelopeSchemaName(hypertable)
	return ee.replicationContext.GetSchemaOrCreate(schemaTopicName, func() schema.Struct {
		return schema.EnvelopeSchema(ee.replicationContext, hypertable, ee.replicationContext)
	})
}

func (ee *EventEmitter) envelopeMessageSchema() schema.Struct {
	schemaTopicName := ee.replicationContext.MessageEnvelopeSchemaName()
	return ee.replicationContext.GetSchemaOrCreate(schemaTopicName, func() schema.Struct {
		return schema.EnvelopeMessageSchema(ee.replicationContext, ee.replicationContext)
	})
}

func (ee *EventEmitter) keySchema(hypertable *systemcatalog.Hypertable) schema.Struct {
	schemaTopicName := ee.replicationContext.HypertableKeySchemaName(hypertable)
	return ee.replicationContext.GetSchemaOrCreate(schemaTopicName, func() schema.Struct {
		return schema.KeySchema(hypertable, ee.replicationContext)
	})
}

func (ee *EventEmitter) emit(xld pglogrepl.XLogData, eventTopicName string, key, envelope schema.Struct) error {
	// Retryable operation
	operation := func() error {
		return ee.sink.Emit(ee.sinkContext, xld.ServerTime, eventTopicName, key, envelope)
	}

	// Run with backoff (it'll automatically reset before starting)
	if err := backoff.Retry(operation, ee.backOff); err != nil {
		return err
	}
	return ee.replicationContext.AcknowledgeProcessed(xld)
}

type eventEmitterEventHandler struct {
	eventEmitter *EventEmitter
}

func (e *eventEmitterEventHandler) OnReadEvent(lsn pglogrepl.LSN, hypertable *systemcatalog.Hypertable,
	_ *systemcatalog.Chunk, newValues map[string]any) error {

	cnValues, err := e.convertValues(hypertable, newValues)
	if err != nil {
		return err
	}

	xld := pglogrepl.XLogData{
		WALStart:     lsn,
		WALData:      []byte{},
		ServerWALEnd: lsn,
		ServerTime:   time.Now(),
	}

	return e.emit0(xld, true, hypertable,
		func(source schema.Struct) schema.Struct {
			return schema.ReadEvent(cnValues, source)
		},
		func() (schema.Struct, error) {
			return e.hypertableEventKey(hypertable, newValues)
		},
	)
}

func (e *eventEmitterEventHandler) OnInsertEvent(xld pglogrepl.XLogData, hypertable *systemcatalog.Hypertable,
	_ *systemcatalog.Chunk, newValues map[string]any) error {

	cnValues, err := e.convertValues(hypertable, newValues)
	if err != nil {
		return err
	}

	return e.emit(xld, hypertable,
		func(source schema.Struct) schema.Struct {
			return schema.CreateEvent(cnValues, source)
		},
		func() (schema.Struct, error) {
			return e.hypertableEventKey(hypertable, newValues)
		},
	)
}

func (e *eventEmitterEventHandler) OnUpdateEvent(xld pglogrepl.XLogData, hypertable *systemcatalog.Hypertable,
	_ *systemcatalog.Chunk, oldValues, newValues map[string]any) error {

	coValues, err := e.convertValues(hypertable, oldValues)
	if err != nil {
		return err
	}
	cnValues, err := e.convertValues(hypertable, newValues)
	if err != nil {
		return err
	}

	return e.emit(xld, hypertable,
		func(source schema.Struct) schema.Struct {
			return schema.UpdateEvent(coValues, cnValues, source)
		},
		func() (schema.Struct, error) {
			return e.hypertableEventKey(hypertable, newValues)
		},
	)
}

func (e *eventEmitterEventHandler) OnDeleteEvent(xld pglogrepl.XLogData, hypertable *systemcatalog.Hypertable,
	_ *systemcatalog.Chunk, oldValues map[string]any, tombstone bool) error {

	coValues, err := e.convertValues(hypertable, oldValues)
	if err != nil {
		return err
	}

	return e.emit(xld, hypertable,
		func(source schema.Struct) schema.Struct {
			return schema.DeleteEvent(coValues, source, tombstone)
		},
		func() (schema.Struct, error) {
			return e.hypertableEventKey(hypertable, oldValues)
		},
	)
}

func (e *eventEmitterEventHandler) OnTruncateEvent(xld pglogrepl.XLogData, hypertable *systemcatalog.Hypertable) error {
	return e.emit(xld, hypertable,
		func(source schema.Struct) schema.Struct {
			return schema.TruncateEvent(source)
		},
		func() (schema.Struct, error) {
			return nil, nil
		},
	)
}

func (e *eventEmitterEventHandler) OnMessageEvent(
	xld pglogrepl.XLogData, msg *pgtypes.LogicalReplicationMessage) error {

	return e.emitMessageEvent(xld, msg, func(source schema.Struct) schema.Struct {
		content := base64.StdEncoding.EncodeToString(msg.Content)
		return schema.MessageEvent(msg.Prefix, content, source)
	})
}

func (e *eventEmitterEventHandler) OnChunkCompressedEvent(
	xld pglogrepl.XLogData, hypertable *systemcatalog.Hypertable, _ *systemcatalog.Chunk) error {

	return e.emit(xld, hypertable,
		func(source schema.Struct) schema.Struct {
			return schema.CompressionEvent(source)
		},
		func() (schema.Struct, error) {
			return e.timescaleEventKey(hypertable)
		},
	)
}

func (e *eventEmitterEventHandler) OnChunkDecompressedEvent(
	xld pglogrepl.XLogData, hypertable *systemcatalog.Hypertable, _ *systemcatalog.Chunk) error {

	return e.emit(xld, hypertable,
		func(source schema.Struct) schema.Struct {
			return schema.DecompressionEvent(source)
		},
		func() (schema.Struct, error) {
			return e.timescaleEventKey(hypertable)
		},
	)
}

func (e *eventEmitterEventHandler) OnRelationEvent(_ pglogrepl.XLogData, _ *pgtypes.RelationMessage) error {
	return nil
}

func (e *eventEmitterEventHandler) OnBeginEvent(_ pglogrepl.XLogData, _ *pgtypes.BeginMessage) error {
	return nil
}

func (e *eventEmitterEventHandler) OnCommitEvent(_ pglogrepl.XLogData, _ *pgtypes.CommitMessage) error {
	return nil
}

func (e *eventEmitterEventHandler) OnTypeEvent(_ pglogrepl.XLogData, _ *pgtypes.TypeMessage) error {
	return nil
}

func (e *eventEmitterEventHandler) OnOriginEvent(_ pglogrepl.XLogData, _ *pgtypes.OriginMessage) error {
	return nil
}

func (e *eventEmitterEventHandler) emit(xld pglogrepl.XLogData, hypertable *systemcatalog.Hypertable,
	eventProvider func(source schema.Struct) schema.Struct, keyProvider func() (schema.Struct, error)) error {

	return e.emit0(xld, false, hypertable, eventProvider, keyProvider)
}

func (e *eventEmitterEventHandler) emit0(xld pglogrepl.XLogData, snapshot bool,
	hypertable *systemcatalog.Hypertable, eventProvider func(source schema.Struct) schema.Struct,
	keyProvider func() (schema.Struct, error)) error {

	transactionId := e.eventEmitter.transactionMonitor.TransactionId()
	envelopeSchema := e.eventEmitter.envelopeSchema(hypertable)
	eventTopicName := e.eventEmitter.replicationContext.EventTopicName(hypertable)

	keyData, err := keyProvider()
	if err != nil {
		return err
	}
	key := schema.Envelope(e.eventEmitter.keySchema(hypertable), keyData)

	event := eventProvider(schema.Source(xld.ServerWALEnd, xld.ServerTime, snapshot,
		hypertable.DatabaseName(), hypertable.SchemaName(), hypertable.TableName(), &transactionId))

	value := schema.Envelope(envelopeSchema, event)

	success, err := e.eventEmitter.filter.Evaluate(hypertable, key, value)
	if err != nil {
		return err
	}

	// If unsuccessful we'll discard the event and not send it to the sink
	if !success {
		return e.eventEmitter.replicationContext.AcknowledgeProcessed(xld)
	}

	return e.eventEmitter.emit(xld, eventTopicName, key, value)
}

func (e *eventEmitterEventHandler) emitMessageEvent(xld pglogrepl.XLogData,
	msg *pgtypes.LogicalReplicationMessage, eventProvider func(source schema.Struct) schema.Struct) error {

	timestamp := time.Now()
	if msg.IsTransactional() {
		timestamp = xld.ServerTime
	}

	var transactionId *uint32
	if msg.IsTransactional() {
		tid := e.eventEmitter.transactionMonitor.TransactionId()
		transactionId = &tid
	}

	envelopeSchema := e.eventEmitter.envelopeMessageSchema()
	messageKeySchema := e.eventEmitter.replicationContext.GetSchema(schema.MessageKeySchemaName)

	source := schema.Source(xld.ServerWALEnd, timestamp, false, "", "", "", transactionId)
	payload := eventProvider(source)
	eventTopicName := e.eventEmitter.replicationContext.MessageTopicName()

	key := schema.Envelope(messageKeySchema, schema.MessageKey(msg.Prefix))
	value := schema.Envelope(envelopeSchema, payload)

	return e.eventEmitter.emit(xld, eventTopicName, key, value)
}

func (e *eventEmitterEventHandler) hypertableEventKey(
	hypertable *systemcatalog.Hypertable, values map[string]any) (schema.Struct, error) {

	columns := make([]systemcatalog.Column, 0)
	for _, column := range hypertable.Columns() {
		if !column.IsPrimaryKey() {
			continue
		}
		columns = append(columns, column)
	}
	return e.convertColumnValues(columns, values)
}

func (e *eventEmitterEventHandler) timescaleEventKey(hypertable *systemcatalog.Hypertable) (schema.Struct, error) {
	return schema.TimescaleKey(hypertable.SchemaName(), hypertable.TableName()), nil
}

func (e *eventEmitterEventHandler) convertValues(
	hypertable *systemcatalog.Hypertable, values map[string]any) (map[string]any, error) {

	return e.convertColumnValues(hypertable.Columns(), values)
}

func (e *eventEmitterEventHandler) convertColumnValues(
	columns []systemcatalog.Column, values map[string]any) (map[string]any, error) {

	if values == nil {
		return nil, nil
	}

	result := make(map[string]any)
	for _, column := range columns {
		if v, present := values[column.Name()]; present {
			converter, err := systemcatalog.ConverterByOID(column.DataType())
			if err != nil {
				return nil, err
			}
			if converter != nil {
				v, err = converter(column.DataType(), v)
				if err != nil {
					return nil, err
				}
			}
			result[column.Name()] = v
		}
	}
	return result, nil
}

type sinkContextImpl struct {
	attributes          map[string]string
	transientAttributes map[string]string
}

func newSinkContextImpl() *sinkContextImpl {
	return &sinkContextImpl{
		attributes:          make(map[string]string),
		transientAttributes: make(map[string]string),
	}
}

func (s *sinkContextImpl) UnmarshalBinary(data []byte) error {
	offset := uint32(0)
	numOfItems := binary.BigEndian.Uint32(data[offset:])
	offset += 4

	for i := uint32(0); i < numOfItems; i++ {
		keyLength := binary.BigEndian.Uint32(data[offset:])
		offset += 4
		valueLength := binary.BigEndian.Uint32(data[offset:])
		offset += 4

		key := string(data[offset : offset+keyLength])
		offset += keyLength

		value := string(data[offset : offset+valueLength])
		offset += valueLength

		s.SetAttribute(key, value)
	}
	return nil
}

func (s *sinkContextImpl) MarshalBinary() (data []byte, err error) {
	data = make([]byte, 0)

	numOfItems := uint32(len(s.attributes))
	data = binary.BigEndian.AppendUint32(data, numOfItems)

	for key, value := range s.attributes {
		keyBytes := []byte(key)
		valueBytes := []byte(value)

		data = binary.BigEndian.AppendUint32(data, uint32(len(keyBytes)))
		data = binary.BigEndian.AppendUint32(data, uint32(len(valueBytes)))

		data = append(data, keyBytes...)
		data = append(data, valueBytes...)
	}
	return data, nil
}

func (s *sinkContextImpl) SetTransientAttribute(key string, value string) {
	s.transientAttributes[key] = value
}

func (s *sinkContextImpl) TransientAttribute(key string) (value string, present bool) {
	value, present = s.transientAttributes[key]
	return
}

func (s *sinkContextImpl) SetAttribute(key string, value string) {
	s.attributes[key] = value
}

func (s *sinkContextImpl) Attribute(key string) (value string, present bool) {
	value, present = s.attributes[key]
	return
}
