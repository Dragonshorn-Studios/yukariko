package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Dragonshorn-Studios/yukariko/internal/config"
	"github.com/Dragonshorn-Studios/yukariko/internal/learn"

	"github.com/spf13/cobra"
)

// webhookSecretRef parses --secret-ref as env:NAME or file:/abs/path.
func webhookSecretRef(raw string) (*config.SecretRef, error) {
	switch {
	case strings.HasPrefix(raw, "env:"):
		name := strings.TrimPrefix(raw, "env:")
		if name == "" {
			return nil, fmt.Errorf("--secret-ref env value must name a variable, e.g. env:AMADEUS_HOOK_SECRET: %w", errUsage)
		}
		return &config.SecretRef{Env: name}, nil
	case strings.HasPrefix(raw, "file:"):
		path := strings.TrimPrefix(raw, "file:")
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("--secret-ref file value must be an absolute path, e.g. file:/etc/yukariko/secrets/hook: %w", errUsage)
		}
		return &config.SecretRef{File: path}, nil
	default:
		return nil, fmt.Errorf("--secret-ref must be env:NAME or file:/abs/path: %w", errUsage)
	}
}

// newWebhookCommand registers, inspects, and removes outbound webhook
// targets (issue #65). Targets live in the YAML configuration — the source
// of truth — and every edit is validated and applied atomically: a failing
// command leaves the file byte-identical.
func (a *App) newWebhookCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "webhook",
		Short: "Manage outbound webhook targets for deployment notifications",
		Long: `Manage outbound webhook targets for deployment notifications.

Targets live in the YAML configuration file; add, list, and remove edit that
file atomically (backup, validate, rename). Every configured target receives
a signed POST when a deployment succeeds or fails.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(a.newWebhookAddCommand())
	cmd.AddCommand(a.newWebhookListCommand())
	cmd.AddCommand(a.newWebhookRemoveCommand())
	return cmd
}

func (a *App) newWebhookAddCommand() *cobra.Command {
	var url, secretRef, timeout string
	var headers []string
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Register a webhook target in the configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if url == "" {
				return fmt.Errorf("--url is required: %w", errUsage)
			}
			hook := config.Webhook{Name: name, URL: url}
			if secretRef != "" {
				ref, err := webhookSecretRef(secretRef)
				if err != nil {
					return err
				}
				hook.SecretRef = ref
			}
			if timeout != "" {
				d, err := time.ParseDuration(timeout)
				if err != nil || d <= 0 {
					return fmt.Errorf("--timeout must be a positive duration such as 15s: %w", errUsage)
				}
				hook.Timeout = config.Duration(d)
			}
			for _, h := range headers {
				hn, hv, ok := strings.Cut(h, "=")
				if !ok || hn == "" || hv == "" {
					return fmt.Errorf("--header must be name=value, got %q: %w", h, errUsage)
				}
				hook.Headers = append(hook.Headers, config.WebhookHeader{Name: hn, Value: hv})
			}

			path, err := a.configPathFor()
			if err != nil {
				return err
			}
			res, err := learn.ApplyConfigEdit(path, func(cfg *config.Config) error {
				for i := range cfg.Webhooks {
					if cfg.Webhooks[i].Name == name {
						return fmt.Errorf("a webhook named %q already exists; names are unique", name)
					}
				}
				cfg.Webhooks = append(cfg.Webhooks, hook)
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "webhook %q added to %s; restart the daemon to deliver (backup: %s).\n", name, path, res.BackupPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "https endpoint that receives signed POSTs")
	cmd.Flags().StringVar(&secretRef, "secret-ref", "", "HMAC signing secret reference: env:NAME or file:/abs/path")
	cmd.Flags().StringVar(&timeout, "timeout", "", "per-delivery timeout, e.g. 15s (default 15s)")
	cmd.Flags().StringArrayVar(&headers, "header", nil, "static header name=value (repeatable)")
	return cmd
}

// webhookView is the display projection of a configured target: tagged
// fields, human-scale timeout.
type webhookView struct {
	Name      string                 `json:"name"`
	URL       string                 `json:"url"`
	Timeout   string                 `json:"timeout"`
	SecretRef *config.SecretRef      `json:"secret_ref,omitempty"`
	Headers   []config.WebhookHeader `json:"headers,omitempty"`
}

func (a *App) newWebhookListCommand() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured webhook targets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := a.configPathFor()
			if err != nil {
				return err
			}
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				// A display projection with json tags: refs are names, so
				// printing them is safe — resolved secret values are never
				// loaded here at all.
				views := make([]webhookView, 0, len(cfg.Webhooks))
				for _, w := range cfg.Webhooks {
					timeout := w.Timeout.D()
					if timeout == 0 {
						timeout = config.DefaultWebhookTimeout.D()
					}
					views = append(views, webhookView{
						Name: w.Name, URL: w.URL,
						Timeout:   timeout.String(),
						SecretRef: w.SecretRef,
						Headers:   w.Headers,
					})
				}
				return enc.Encode(views)
			}
			if len(cfg.Webhooks) == 0 {
				fmt.Fprintln(out, "no webhooks; add one with `yukariko webhook add <name> --url <url>`.")
				return nil
			}
			fmt.Fprintf(out, "%-20s %-8s %s\n", "NAME", "TIMEOUT", "URL")
			for _, w := range cfg.Webhooks {
				timeout := w.Timeout.D()
				if timeout == 0 {
					timeout = config.DefaultWebhookTimeout.D()
				}
				fmt.Fprintf(out, "%-20s %-8s %s\n", w.Name, timeout, w.URL)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print JSON output")
	return cmd
}

func (a *App) newWebhookRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a webhook target from the configuration",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			path, err := a.configPathFor()
			if err != nil {
				return err
			}
			res, err := learn.ApplyConfigEdit(path, func(cfg *config.Config) error {
				kept := cfg.Webhooks[:0:0]
				for _, w := range cfg.Webhooks {
					if w.Name != name {
						kept = append(kept, w)
					}
				}
				if len(kept) == len(cfg.Webhooks) {
					return fmt.Errorf("no webhook named %q (see `yukariko webhook list`)", name)
				}
				cfg.Webhooks = kept
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "webhook %q removed from %s (backup: %s).\n", name, path, res.BackupPath)
			return nil
		},
	}
}
