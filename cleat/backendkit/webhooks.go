package backendkit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// ErrWebhookNotFound is returned by WebhookByName when the tenant has no webhook with that name. A name that
// could never exist is not this: the worker refuses it with a 400, which arrives as an *UpstreamStatusError.
var ErrWebhookNotFound = errors.New("no webhook has that name")

// Webhook is a webhook registered with the notifications plugin, as GET /webhooks returns it. The signing
// secret is never returned by the worker; SecretConfigured says one is set.
type Webhook struct {
	ID               string   `json:"id"`
	URL              string   `json:"url"`
	Name             string   `json:"name"` // "" for a webhook that has none
	Events           []string `json:"events"`
	Enabled          bool     `json:"enabled"`
	SecretConfigured bool     `json:"secret_configured"`
}

// WebhookByName finds the tenant's webhook with the given name, so an app can find its own webhook by a
// stable identity instead of by its URL, which changes, or by its events, which mean something else
// (cleat#3538). A webhook's name is unique within its tenant among webhooks that have not been deleted, so
// there is at most one. ErrWebhookNotFound means there is none.
//
// It reaches the notifications plugin's GET /webhooks?name=<name> through PluginRoute, so it rides the
// client's own transport and returns the same typed errors as every other method.
func (c *Client) WebhookByName(ctx context.Context, name string) (*Webhook, error) {
	var found []Webhook
	if err := c.PluginRoute(ctx, http.MethodGet, "/webhooks?name="+url.QueryEscape(name), nil, &found); err != nil {
		return nil, err
	}
	switch len(found) {
	case 0:
		return nil, ErrWebhookNotFound
	case 1:
		return &found[0], nil
	}
	// The worker's index makes this impossible. If it ever happens, picking one would be a guess about
	// which webhook an app means, and the app would deliver through it.
	return nil, fmt.Errorf("the worker returned %d webhooks named %q; a name is unique, so this is a worker bug", len(found), name)
}
