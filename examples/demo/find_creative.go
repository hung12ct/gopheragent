package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/hung12ct/gopheragent/pkg/tools"
)

// FindTopCreativeTool is a concrete Tool implementation for fetching MySQL data
type FindTopCreativeTool struct {
	DB *sql.DB
}

// NewFindTopCreativeTool creates a new instance of the tool
func NewFindTopCreativeTool(db *sql.DB) *FindTopCreativeTool {
	return &FindTopCreativeTool{DB: db}
}

func (t *FindTopCreativeTool) Name() string {
	return "get_top_creatives"
}

func (t *FindTopCreativeTool) Description() string {
	return "Fetches the list of top performing ad creatives based on impressions/clicks within a specific timeframe."
}

func (t *FindTopCreativeTool) RequiresConfirmation() bool {
	return false
}

func (t *FindTopCreativeTool) ParametersSchema() tools.ToolSchema {
	return tools.ToolSchema{
		Type: "object",
		Properties: map[string]interface{}{
			"platform":   map[string]interface{}{"type": "string", "enum": []string{"facebook", "google", "tiktok"}, "description": "The advertising platform"},
			"last_n_days": map[string]interface{}{"type": "integer", "description": "Number of recent days to fetch data for (default 30)"},
		},
		Required: []string{"platform", "last_n_days"},
	}
}

// json mapping struct
type findCreativeArgs struct {
	Platform  string `json:"platform"`
	LastNDays int    `json:"last_n_days"`
}

func (t *FindTopCreativeTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args findCreativeArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// This is just a stub logic if DB is nil (for boilerplate runability)
	if t.DB == nil {
		return fmt.Sprintf(`[{"id":"demo1","url":"https://demo.com/1","impressions":1000, "platform": "%s"}]`, args.Platform), nil
	}

	// Safe Prepared Statement prevents SQL injection
	query := `
		SELECT creative_id, creative_url, impressions
		FROM performance_metrics
		WHERE platform = ? AND created_at >= DATE_SUB(NOW(), INTERVAL ? DAY)
		ORDER BY impressions DESC LIMIT 5
	`

	rows, err := t.DB.QueryContext(ctx, query, args.Platform, args.LastNDays)
	if err != nil {
		return "", fmt.Errorf("db error: %w", err)
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var id, url string
		var imp int
		if err := rows.Scan(&id, &url, &imp); err == nil {
			results = append(results, map[string]interface{}{
				"id": id, "url": url, "impressions": imp,
			})
		}
	}

	resBytes, _ := json.Marshal(results)
	return string(resBytes), nil
}
