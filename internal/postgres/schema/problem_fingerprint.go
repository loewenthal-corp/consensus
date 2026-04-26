package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type ProblemFingerprint struct {
	ent.Schema
}

func (ProblemFingerprint) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Unique(),
		field.String("tenant_key").
			Default("default").
			NotEmpty().
			MaxLen(255),
		field.UUID("insight_id", uuid.UUID{}),
		field.String("error_hash").
			Optional().
			MaxLen(255),
		field.String("command").
			Optional().
			MaxLen(255),
		field.String("toolchain").
			Optional().
			MaxLen(255),
		field.String("service").
			Optional().
			MaxLen(255),
		field.String("repo_path_pattern").
			Optional().
			MaxLen(500),
		field.String("environment").
			Optional().
			MaxLen(255),
		field.JSON("dependency_versions", map[string]string{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

func (ProblemFingerprint) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_key"),
		index.Fields("tenant_key", "insight_id"),
		index.Fields("tenant_key", "error_hash"),
	}
}
