package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type Setting struct {
	ent.Schema
}

func (Setting) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Unique(),
		field.String("tenant_key").
			Default("default").
			NotEmpty().
			MaxLen(255),
		field.String("key").
			NotEmpty().
			MaxLen(255),
		field.JSON("value", map[string]string{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

func (Setting) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_key", "key").Unique(),
	}
}
