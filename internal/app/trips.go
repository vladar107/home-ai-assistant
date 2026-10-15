package app

import (
	"context"

	"github.com/vladar107/home-ai-assistant/internal/conversation"
)

type tripScanChecker struct {
	conversation *conversation.Service
}

func (c tripScanChecker) RunCheck(ctx context.Context, principalID, message string) (string, string, error) {
	reply, err := c.conversation.RunCheck(ctx, principalID, message)
	return reply.Text, reply.ErrorKind, err
}
