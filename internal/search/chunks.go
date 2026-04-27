package search

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/loewenthal-corp/consensus/internal/postgres"
)

type insightChunk struct {
	Kind           string
	Ordinal        int
	Text           string
	SearchDocument string
	ContentHash    string
}

func buildInsightChunks(item *postgres.Insight) []insightChunk {
	if item == nil {
		return nil
	}

	core := compactSections(
		item.Title,
		item.Problem,
		item.Answer,
		item.Action,
		item.Detail,
	)
	core = append(core, mapValues(item.Example)...)
	core = append(core, item.Tags...)
	core = append(core, linkValues(item.Links)...)

	searchDocument := weightedDocument(item)
	if searchDocument == "" {
		searchDocument = strings.Join(core, "\n\n")
	}

	hash := sha256.Sum256([]byte(searchDocument))
	return []insightChunk{{
		Kind:           "main",
		Ordinal:        0,
		Text:           strings.Join(core, "\n\n"),
		SearchDocument: searchDocument,
		ContentHash:    fmt.Sprintf("%x", hash[:]),
	}}
}

func weightedDocument(item *postgres.Insight) string {
	var sections []string
	addWeighted := func(weight int, values ...string) {
		text := strings.Join(compactSections(values...), " ")
		if text == "" {
			return
		}
		for range weight {
			sections = append(sections, text)
		}
	}

	addWeighted(4, item.Title, item.Problem)
	addWeighted(3, mapValues(item.Example)...)
	addWeighted(2, item.Answer, item.Action)
	addWeighted(2, strings.Join(item.Tags, " "))
	addWeighted(1, item.Detail)
	addWeighted(1, linkValues(item.Links)...)

	return strings.Join(sections, "\n")
}

func compactSections(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}

func mapValues(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(values)*2)
	for _, key := range keys {
		if strings.TrimSpace(key) != "" {
			out = append(out, key)
		}
		if value := strings.TrimSpace(values[key]); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func linkValues(links []map[string]string) []string {
	out := make([]string, 0, len(links)*4)
	for _, link := range links {
		out = append(out,
			link["title"],
			link["description"],
			link["excerpt"],
			link["uri"],
		)
	}
	return compactSections(out...)
}
