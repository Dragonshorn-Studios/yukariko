package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/auth"
	"github.com/Dragonshorn-Studios/yukariko/internal/daemon"
	"github.com/Dragonshorn-Studios/yukariko/internal/store"

	"github.com/spf13/cobra"
)

// apiKeyNamePattern matches the human-readable key names: a unique label
// safe for columns, logs, and file-adjacent contexts.
var apiKeyNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// newAPIKeyCommand is the machine-credential management group (issue #64).
// Keys live in the store — hash-only — and the full token is shown exactly
// once at creation. Nothing here touches the HTTP surface or the config.
func (a *App) newAPIKeyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apikey",
		Short: "Manage API keys for machine access to the read-only API",
		Long: `Manage API keys for machine access to the read-only API.

Keys are stored by SHA-256 hash only; the bearer token is printed exactly
once at creation. Enabling auth.api_keys in the configuration makes every
/api request require a valid key (an OIDC session also works when OIDC is
configured). The dashboard /ui is never unlocked by API keys.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(a.newAPIKeyCreateCommand())
	cmd.AddCommand(a.newAPIKeyListCommand())
	cmd.AddCommand(a.newAPIKeyRevokeCommand())
	return cmd
}

func (a *App) newAPIKeyCreateCommand() *cobra.Command {
	var name, expires string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Generate a new API key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !apiKeyNamePattern.MatchString(name) {
				return fmt.Errorf("--name must be 1-64 characters of letters, digits, _ or - (no leading - or _): %w", errUsage)
			}
			var ttl time.Duration
			if expires != "" {
				parsed, err := time.ParseDuration(expires)
				if err != nil || parsed <= 0 {
					return fmt.Errorf("--expires must be a positive duration such as 720h: %w", errUsage)
				}
				ttl = parsed
			}
			opts, err := a.daemonOptions()
			if err != nil {
				return err
			}
			asm, err := daemon.Assemble(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer asm.Store.Close()
			ctx := cmd.Context()

			if _, ok, err := asm.Store.APIKeyByName(ctx, name); err != nil {
				return err
			} else if ok {
				return fmt.Errorf("an api key named %q already exists; names are unique", name)
			}

			token, hash := auth.GenerateAPIKey()
			now := time.Now().UTC()
			k := store.APIKey{Name: name, TokenHash: hash, CreatedAt: now}
			var expiresAt *time.Time
			if ttl > 0 {
				at := now.Add(ttl)
				k.ExpiresAt, expiresAt = &at, &at
			}
			if err := asm.Store.CreateAPIKey(ctx, k); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				type created struct {
					Name      string     `json:"name"`
					Token     string     `json:"token"`
					ExpiresAt *time.Time `json:"expires_at,omitempty"`
				}
				return enc.Encode(created{Name: name, Token: token, ExpiresAt: expiresAt})
			}
			fmt.Fprintf(out, "API key %q created. The bearer token is:\n\n  %s\n\nStore it now: this token is shown exactly once and cannot be recovered.\n", name, token)
			if ttl > 0 {
				fmt.Fprintf(out, "It expires at %s.\n", expiresAt.UTC().Format(time.RFC3339))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "unique human-readable name for the key")
	cmd.Flags().StringVar(&expires, "expires", "", "optional expiry as a Go duration, e.g. 720h")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print JSON output")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

// apiKeyView is the display projection of a key: no token hash, a derived
// status, and RFC3339 stamps.
type apiKeyView struct {
	Name       string     `json:"name"`
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

func apiKeyViews(keys []store.APIKey, now time.Time) []apiKeyView {
	out := make([]apiKeyView, 0, len(keys))
	for _, k := range keys {
		status := "active"
		switch {
		case k.RevokedAt != nil:
			status = "revoked"
		case k.ExpiresAt != nil && !k.ExpiresAt.After(now):
			status = "expired"
		}
		out = append(out, apiKeyView{
			Name: k.Name, ID: k.ID, Status: status,
			CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt,
			ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt,
		})
	}
	return out
}

func (a *App) newAPIKeyListCommand() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List API keys and their status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := a.daemonOptions()
			if err != nil {
				return err
			}
			asm, err := daemon.Assemble(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer asm.Store.Close()

			keys, err := asm.Store.ListAPIKeys(cmd.Context())
			if err != nil {
				return err
			}
			views := apiKeyViews(keys, time.Now().UTC())
			out := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(views)
			}
			if len(views) == 0 {
				fmt.Fprintln(out, "no api keys; create one with `yukariko apikey create --name <name>`.")
				return nil
			}
			fmt.Fprintf(out, "%-20s %-10s %-25s %-25s %s\n", "NAME", "STATUS", "CREATED", "LAST USED", "EXPIRES")
			for _, v := range views {
				fmt.Fprintf(out, "%-20s %-10s %-25s %-25s %s\n",
					v.Name, v.Status,
					v.CreatedAt.Format(time.RFC3339),
					orDash(v.LastUsedAt), orDash(v.ExpiresAt))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print JSON output")
	return cmd
}

func orDash(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format(time.RFC3339)
}

func (a *App) newAPIKeyRevokeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "revoke <name|id>",
		Short: "Revoke an API key; it stops working immediately",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			opts, err := a.daemonOptions()
			if err != nil {
				return err
			}
			asm, err := daemon.Assemble(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer asm.Store.Close()
			ctx := cmd.Context()

			// Look up first so "unknown" and "already revoked" get distinct,
			// actionable messages.
			k, ok, err := asm.Store.APIKeyByName(ctx, ref)
			if err != nil {
				return err
			}
			if !ok {
				k, ok, err = asm.Store.APIKeyByID(ctx, ref)
				if err != nil {
					return err
				}
			}
			if !ok {
				return fmt.Errorf("no api key named or with id %q (see `yukariko apikey list`)", ref)
			}
			if k.RevokedAt != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "api key %q is already revoked (at %s).\n", k.Name, k.RevokedAt.Format(time.RFC3339))
				return nil
			}
			now := time.Now().UTC()
			// Revoke by the resolved row's id: a single namespace, so a key
			// whose name happens to equal another key's id can never be
			// swept up (or stand in) for the target.
			if _, err := asm.Store.RevokeAPIKey(ctx, k.ID, now); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "api key %q revoked; it stops working immediately.\n", k.Name)
			return nil
		},
	}
	return cmd
}
