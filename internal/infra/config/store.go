package config

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"

	"memorize/internal/domain"
)

const (
	CfgActiveProvider           = "active_provider"
	CfgGGUFEndpoint             = "gguf_endpoint"
	CfgGGUFModel                = "gguf_model"
	CfgIncludePersonalInProject = "include_personal_in_project"
	CfgDefaultAccount           = "default_account"
	CfgPresetCategories         = "preset_categories"
	CfgRulePackAuthor           = "rulepack_author"

	CfgProgressRetentionDays   = "progress_retention_days"
	CfgGuardrailRetentionDays  = "guardrail_retention_days"
	CfgGuardrailIncludeBlocked = "guardrail_retention_include_blocked"
	// CfgProgressPrunedMemHWM stores the highest pruned memory ID per scope to prevent resurrection.
	CfgProgressPrunedMemHWM = "progress_pruned_mem_hwm:"
)

func LoadRetentionPolicy(ctx context.Context, db *sql.DB) domain.RetentionPolicy {
	cfg := GetConfigMap(ctx, db)
	progDays := 0
	if raw, ok := cfg[CfgProgressRetentionDays]; ok && strings.TrimSpace(raw) != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			progDays = domain.NormalizeRetentionDays(v)
		}
	}
	grDays := 0
	if raw, ok := cfg[CfgGuardrailRetentionDays]; ok && strings.TrimSpace(raw) != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			grDays = domain.NormalizeRetentionDays(v)
		}
	}
	return domain.RetentionPolicy{
		ProgressDays:            progDays,
		GuardrailDays:           grDays,
		GuardrailIncludeBlocked: cfg[CfgGuardrailIncludeBlocked] == "1",
	}
}

func GetConfigMap(ctx context.Context, db *sql.DB) map[string]string {
	out := make(map[string]string)
	if db == nil {
		return out
	}
	rows, err := db.QueryContext(ctx, `SELECT key, val FROM config`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			out[k] = v
		}
	}
	return out
}

// SaveConfig atomically upserts key/value configuration pairs and re-enforces 0600 file permissions.
func SaveConfig(ctx context.Context, db *sql.DB, dbPath string, pairs map[string]string) error {
	if db == nil {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range pairs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config(key, val) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET val = excluded.val`,
			k, strings.TrimSpace(v)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	RestrictDBPermissions(dbPath)
	return nil
}

func SyncManagedGGUFEndpoint(ctx context.Context, db *sql.DB, dbPath, ep string) error {
	if db == nil || strings.TrimSpace(ep) == "" {
		return nil
	}
	if strings.TrimSpace(os.Getenv("LOCAL_GGUF_URL")) != "" {
		return nil
	}
	raw := GetConfigMap(ctx, db)
	cur := strings.TrimSpace(raw[CfgGGUFEndpoint])
	if cur != "" && !domain.IsLoopbackEndpoint(cur) {
		return nil
	}
	trimmed := strings.TrimRight(strings.TrimSpace(ep), "/")
	if strings.TrimRight(cur, "/") == trimmed {
		return nil
	}
	return SaveConfig(domain.WithAdminBypass(ctx), db, dbPath, map[string]string{CfgGGUFEndpoint: trimmed})
}
