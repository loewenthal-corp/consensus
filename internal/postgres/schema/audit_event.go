package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type AuditEvent struct {
	ent.Schema
}

func (AuditEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Unique(),
		field.String("tenant_key").
			Default("default").
			NotEmpty().
			MaxLen(255),
		field.UUID("actor_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.String("auth_mode").
			Default("none").
			NotEmpty().
			MaxLen(100),
		field.String("action").
			NotEmpty().
			MaxLen(255),
		field.String("target_kind").
			Optional().
			MaxLen(100),
		field.String("target_id").
			Optional().
			MaxLen(255),
		field.JSON("metadata", map[string]string{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

func (AuditEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_key"),
		index.Fields("tenant_key", "action"),
		index.Fields("tenant_key", "target_kind", "target_id"),
	}
}
