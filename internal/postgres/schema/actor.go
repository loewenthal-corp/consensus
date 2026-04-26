package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type Actor struct {
	ent.Schema
}

func (Actor) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Unique(),
		field.String("tenant_key").
			Default("default").
			NotEmpty().
			MaxLen(255),
		field.String("kind").
			Default("agent").
			NotEmpty().
			MaxLen(100),
		field.String("name").
			Default("anonymous").
			NotEmpty().
			MaxLen(255),
		field.String("external_id").
			Optional().
			Nillable().
			MaxLen(255),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

func (Actor) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_key"),
		index.Fields("tenant_key", "external_id"),
	}
}
