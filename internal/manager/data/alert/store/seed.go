package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	model "github.com/ongridio/ongrid/internal/manager/model/alert"
	"github.com/ongridio/ongrid/internal/pkg/config"
	"github.com/ongridio/ongrid/internal/pkg/errs"
)

// SeedChannelsFromConfig keeps notification_channels in sync with the env
// configuration on every boot. Channels disabled in cfg are upserted with
// enabled=false rather than deleted, so previously-recorded deliveries keep
// their channel_id FK.
//
// Endpoint URLs and webhook secrets land in config_json so the operator can
// inspect a single column when debugging — handy for runbooks.
//
// This function is idempotent: every boot rewrites config_json + enabled for
// rows whose name matches; new names are inserted, names not present in cfg
// are left untouched (config-managed channels coexist with future UI-managed
// rows from PR-D).
func SeedChannelsFromConfig(ctx context.Context, repo *Repo, cfg config.NotificationConfig) error {
	if repo == nil {
		return nil
	}
	candidates := []seedCandidate{
		{Name: cfg.Webhook.Name, Type: model.ChannelTypeWebhook, Enabled: cfg.Webhook.Enabled, URL: cfg.Webhook.URL, Secret: cfg.Webhook.Secret},
		{Name: cfg.Slack.Name, Type: model.ChannelTypeSlack, Enabled: cfg.Slack.Enabled, URL: cfg.Slack.URL, Secret: cfg.Slack.Secret},
		{Name: cfg.Feishu.Name, Type: model.ChannelTypeFeishu, Enabled: cfg.Feishu.Enabled, URL: cfg.Feishu.URL, Secret: cfg.Feishu.Secret},
		{Name: cfg.DingTalk.Name, Type: model.ChannelTypeDingTalk, Enabled: cfg.DingTalk.Enabled, URL: cfg.DingTalk.URL, Secret: cfg.DingTalk.Secret},
	}
	for _, c := range candidates {
		if c.Name == "" {
			continue
		}
		// Factory default ships ZERO notification channels: only seed a
		// candidate the operator actually configured (enabled, or a URL
		// set via env). Skipping the empty placeholders keeps a fresh
		// install's channel list clean — the operator adds channels from
		// the UI when they want them. (Without this, the four type
		// placeholders showed up disabled+empty and even produced no-op
		// "notification_sent" timeline noise.)
		//
		// **But only skip when there is nothing to update.** Skipping a
		// name that already has a row means "disable this channel via
		// env" silently does nothing: the row keeps enabled=true and the
		// old endpoint, so alerts keep going to an address the operator
		// believes they turned off.
		//
		// That failure is invisible from every angle an operator would
		// check — .env is right, the container's env is right, and the
		// only place the truth lives is a DB row nobody thought to look
		// at. Deliveries just keep flowing to the old endpoint.
		if !c.Enabled && strings.TrimSpace(c.URL) == "" {
			existing, err := repo.GetChannelByName(ctx, c.Name)
			if errors.Is(err, errs.ErrNotFound) {
				// Nothing to sync — this is the fresh-install path the
				// skip was written for.
				continue
			}
			if err != nil {
				return fmt.Errorf("look up channel %q: %w", c.Name, err)
			}
			if !existing.Enabled {
				// Already off; leave config_json alone so a UI-managed
				// endpoint isn't wiped by an empty env var.
				continue
			}
			if err := repo.UpdateChannelEnabled(ctx, existing.ID, false); err != nil {
				return fmt.Errorf("disable channel %q: %w", c.Name, err)
			}
			continue
		}
		ch := &model.Channel{
			Name:        c.Name,
			ChannelType: c.Type,
			Enabled:     c.Enabled,
			ConfigJSON:  encodeChannelConfig(c.URL, c.Secret),
		}
		if _, err := repo.UpsertChannelByName(ctx, ch); err != nil {
			return fmt.Errorf("upsert channel %q: %w", c.Name, err)
		}
	}
	// Idempotent cleanup: any pre-existing channel rows of the legacy
	// "log" type get soft-deleted on every boot. The legacy log sender
	// was removed; rules pinned to a log channel via
	// notify_channel_ids_json fall through to the global default
	// channel set. Soft delete (gorm DeletedAt) preserves
	// notification_deliveries audit history.
	if err := repo.PurgeLegacyLogChannels(ctx); err != nil {
		return fmt.Errorf("purge legacy log channels: %w", err)
	}
	return nil
}

type seedCandidate struct {
	Name    string
	Type    string
	Enabled bool
	URL     string
	Secret  string
}

func encodeChannelConfig(url, secret string) string {
	out := map[string]string{}
	if url != "" {
		out["endpoint"] = url
	}
	if secret != "" {
		out["secret_set"] = "true"
	}
	if len(out) == 0 {
		return "{}"
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "{}"
	}
	return string(b)
}
