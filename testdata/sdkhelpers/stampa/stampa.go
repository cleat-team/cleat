// Package stampa is one half of a name collision (cleat#3814 review): its Client.Stamp makes a host call.
//
// stampb declares a Client.Stamp of the same shape that makes none. The build must not take one for the other.
package stampa

import "github.com/cleat-team/cleat/cleat"

type Client struct{}

func NewClient() *Client { return &Client{} }

// Stamp reads the durable clock.
func (c *Client) Stamp(h cleat.HostCalls) int64 { return h.NowMs() }
