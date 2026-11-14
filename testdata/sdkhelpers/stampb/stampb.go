// Package stampb is the other half of a name collision (cleat#3814 review): a Client.Stamp that makes NO host
// call, in a package whose path sorts after stampa's.
package stampb

import "github.com/cleat-team/cleat/cleat"

type Client struct{}

func NewClient() *Client { return &Client{} }

// Stamp is the same declaration as stampa's, minus the host call.
func (c *Client) Stamp(h cleat.HostCalls) int64 { return 0 }
