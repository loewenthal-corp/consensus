package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type Insight struct {
	ent.Schema
}

func (Insight) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Unique(),
		field.String("tenant_key").
			Default("default").
			NotEmpty().
			MaxLen(255),
		field.String("title").
			NotEmpty().
			MaxLen(255),
		field.Text("problem").
			Optional(),
		field.String("answer").
			NotEmpty().
			MaxLen(1000),
		field.JSON("example", map[string]string{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Text("detail").
			Optional(),
		field.Text("action").
			Optional(),
		field.String("kind").
			Default("insight").
			NotEmpty().
			MaxLen(100),
		field.JSON("tags", []string{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.JSON("context", map[string]string{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.JSON("links", []map[string]string{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.UUID("created_by_actor_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.String("source_run_id").
			Optional().
			Nillable().
			MaxLen(255),
		field.String("review_state").
			Default("approved").
			NotEmpty().
			MaxLen(100),
		field.String("lifecycle_state").
			Default("active").
			NotEmpty().
			MaxLen(100),
		field.UUID("superseded_by_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.Time("last_confirmed_at").
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

func (Insight) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_key"),
		index.Fields("tenant_key", "review_state"),
		index.Fields("tenant_key", "lifecycle_state"),
	}
}
