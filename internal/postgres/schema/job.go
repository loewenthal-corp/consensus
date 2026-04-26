package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type Job struct {
	ent.Schema
}

func (Job) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Unique(),
		field.String("tenant_key").
			Default("default").
			NotEmpty().
			MaxLen(255),
		field.String("kind").
			NotEmpty().
			MaxLen(100),
		field.String("status").
			Default("pending").
			NotEmpty().
			MaxLen(100),
		field.JSON("payload", map[string]string{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Text("last_error").
			Optional(),
		field.Int("attempts").
			Default(0),
		field.Time("run_at").
			Default(time.Now),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

func (Job) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_key"),
		index.Fields("tenant_key", "status"),
		index.Fields("tenant_key", "kind", "status"),
	}
}
