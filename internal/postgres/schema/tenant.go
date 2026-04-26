package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type Tenant struct {
	ent.Schema
}

func (Tenant) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Unique(),
		field.String("key").
			Default("default").
			NotEmpty().
			MaxLen(255).
			Comment("Stable organization key. Authless mode uses default."),
		field.String("name").
			Default("Default").
			NotEmpty().
			MaxLen(255),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

func (Tenant) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("key").Unique(),
	}
}
