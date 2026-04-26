package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type GraphEdge struct {
	ent.Schema
}

func (GraphEdge) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Unique(),
		field.String("tenant_key").
			Default("default").
			NotEmpty().
			MaxLen(255),
		field.UUID("from_id", uuid.UUID{}),
		field.UUID("to_id", uuid.UUID{}),
		field.String("relationship").
			NotEmpty().
			MaxLen(100),
		field.Text("rationale").
			Optional(),
		field.String("review_state").
			Default("approved").
			NotEmpty().
			MaxLen(100),
		field.Time("tombstoned_at").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

func (GraphEdge) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_key"),
		index.Fields("tenant_key", "from_id"),
		index.Fields("tenant_key", "to_id"),
		index.Fields("tenant_key", "from_id", "to_id", "relationship"),
	}
}
