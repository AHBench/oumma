package events

import (
	"context"
	"log/slog"
	"time"

	"oumma/internal/domain"
)

// Dispatcher routes workflow events to the correct Kafka topic.
// Implementations must be safe for concurrent use.
type Dispatcher interface {
	// DispatchTaskEvent sends a user-driven task mutation event to flowiq-task-stream.
	DispatchTaskEvent(ctx context.Context, evt TaskEvent)
	// DispatchProcessEvent sends an LLM-driven transition event to flowiq-process-stream.
	DispatchProcessEvent(ctx context.Context, evt ProcessEvent)
	// Close flushes pending messages and releases resources.
	Close() error
}

// ── Kafka dispatcher ─────────────────────────────────────────────────────────

// KafkaDispatcher publishes events to Kafka asynchronously.
type KafkaDispatcher struct {
	producer *asyncProducer
	logger   *slog.Logger
}

// NewKafkaDispatcher creates a KafkaDispatcher connected to the given broker
// addresses (host:port). The returned dispatcher is ready to use immediately;
// the underlying writer reconnects on failure.
func NewKafkaDispatcher(brokers []string, logger *slog.Logger) *KafkaDispatcher {
	return &KafkaDispatcher{
		producer: newAsyncProducer(brokers, logger),
		logger:   logger,
	}
}

func (d *KafkaDispatcher) DispatchTaskEvent(_ context.Context, evt TaskEvent) {
	d.producer.publish(TopicTaskStream, evt.TaskID, evt)
}

func (d *KafkaDispatcher) DispatchProcessEvent(_ context.Context, evt ProcessEvent) {
	d.producer.publish(TopicProcessStream, evt.ProcessInstanceID, evt)
}

func (d *KafkaDispatcher) Close() error {
	return d.producer.close()
}

// ── Log-only fallback dispatcher ─────────────────────────────────────────────

// LogDispatcher is a no-op fallback used when Kafka is not configured.
// Events are emitted as structured log lines so the system remains observable
// even in environments without a Kafka broker.
type LogDispatcher struct {
	logger *slog.Logger
}

// NewLogDispatcher creates a LogDispatcher that writes events to logger.
func NewLogDispatcher(logger *slog.Logger) *LogDispatcher {
	return &LogDispatcher{logger: logger}
}

func (d *LogDispatcher) DispatchTaskEvent(_ context.Context, evt TaskEvent) {
	d.logger.Info("event:task",
		"event_id", evt.EventID,
		"operation", string(evt.Operation),
		"process_id", evt.ProcessID,
		"task_id", evt.TaskID,
		"fields", evt.Fields,
	)
}

func (d *LogDispatcher) DispatchProcessEvent(_ context.Context, evt ProcessEvent) {
	d.logger.Info("event:process",
		"event_id", evt.EventID,
		"process_instance_id", evt.ProcessInstanceID,
		"from_task", evt.StatusChange.FromTask,
		"to_task", evt.StatusChange.ToTask,
		"reasoning", evt.StatusChange.Reasoning,
	)
}

func (d *LogDispatcher) Close() error { return nil }

// ── Event factory helpers ─────────────────────────────────────────────────────

// NewTaskEvent constructs a TaskEvent with a generated ID and current timestamp.
func NewTaskEvent(op OperationType, processID, taskID string, fields map[string]any) TaskEvent {
	return TaskEvent{
		EventID:    domain.NewUUID(),
		OccurredAt: time.Now().UTC(),
		Operation:  op,
		ProcessID:  processID,
		TaskID:     taskID,
		Fields:     fields,
	}
}

// NewProcessEvent constructs a ProcessEvent with a generated ID and current timestamp.
func NewProcessEvent(processInstanceID, fromTask, toTask, reasoning string, llmOutput any) ProcessEvent {
	return ProcessEvent{
		EventID:           domain.NewUUID(),
		OccurredAt:        time.Now().UTC(),
		ProcessInstanceID: processInstanceID,
		StatusChange: StatusChange{
			FromTask:  fromTask,
			ToTask:    toTask,
			Reasoning: reasoning,
		},
		LLMOutput: llmOutput,
	}
}
