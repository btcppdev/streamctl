package btcppclient

import (
	"context"
	"net/http"
	"time"
)

type PublishingStream struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Conference string     `json:"conference"`
	State      string     `json:"state"`
	At         *time.Time `json:"at"`
	End        *time.Time `json:"end"`
}

func (c *Client) PutPublishingStreams(ctx context.Context, streams []PublishingStream) error {
	var out map[string]int
	return c.do(ctx, http.MethodPut, "/api/v1/publishing/streams", struct {
		Streams []PublishingStream `json:"streams"`
	}{streams}, &out)
}
