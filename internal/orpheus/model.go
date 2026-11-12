package orpheus

import (
	"context"

	api "github.com/orpheus-agents/orpheus/client"
)

func (c *Client) Profiles(ctx context.Context) ([]api.Profile, error) {
	catalog, err := decode[api.Profiles](c.api.GetProfiles(ctx)) //nolint:bodyclose // decode closes the response body.
	return catalog.Items, err
}
