package consensus

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	consensusv1 "github.com/loewenthal-corp/consensus/internal/gen/consensus/v1"
)

func TestService_RecordOutcomeRejectsNotApplicable(t *testing.T) {
	svc := NewService(nil)

	_, err := svc.RecordOutcome(context.Background(), &consensusv1.InsightServiceRecordOutcomeRequest{
		InsightRef: "00000000-0000-0000-0000-000000000000",
		Outcome:    "not_applicable",
	})

	require.Error(t, err)
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	require.Contains(t, err.Error(), `unsupported outcome "not_applicable"`)
}
