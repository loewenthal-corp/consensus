package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type Vote struct {
	ent.Schema
}

func (Vote) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Unique(),
		field.String("tenant_key").
			Default("default").
			NotEmpty().
			MaxLen(255),
		field.UUID("insight_id", uuid.UUID{}),
		field.UUID("actor_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("problem_fingerprint_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.String("outcome").
			NotEmpty().
			MaxLen(100),
		field.Float("confidence").
			Default(0),
		field.Text("rationale").
			Optional(),
		field.String("idempotency_key").
			Optional().
			Nillable().
			MaxLen(255),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

func (Vote) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_key"),
		index.Fields("tenant_key", "insight_id"),
		index.Fields("tenant_key", "outcome"),
	}
}
