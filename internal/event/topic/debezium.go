package topic

import (
	"fmt"
	"github.com/noctarius/event-stream-prototype/internal/systemcatalog/model"
)

type DebeziumNamingStrategy struct {
}

func (d *DebeziumNamingStrategy) EventTopicName(topicPrefix string, hypertable *model.Hypertable) string {
	return fmt.Sprintf("%s.%s.%s", topicPrefix, hypertable.SchemaName(), hypertable.HypertableName())
}

func (d *DebeziumNamingStrategy) SchemaTopicName(topicPrefix string, hypertable *model.Hypertable) string {
	return fmt.Sprintf("%s.%s.%s", topicPrefix, hypertable.SchemaName(), hypertable.HypertableName())
}
