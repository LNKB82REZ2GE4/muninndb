package grpc_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/scrypster/muninndb/internal/auth"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
)

func TestActivate_MultiVault_MergesAndTagsVault(t *testing.T) {
	eng := &mockEngine{
		activateMultiFn: func(ctx context.Context, reqs []*pb.ActivateRequest, weights []float64) (*pb.ActivateResponse, error) {
			if len(reqs) != 2 {
				t.Fatalf("ActivateMulti called with %d requests, want 2", len(reqs))
			}
			if len(weights) != 2 || weights[0] != 0.67 || weights[1] != 0.33 {
				t.Errorf("weights = %v, want [0.67 0.33]", weights)
			}
			return &pb.ActivateResponse{
				TotalFound:     2,
				Activations:    []pb.ActivationItem{{ID: "e1", Vault: reqs[0].Vault}, {ID: "e2", Vault: reqs[1].Vault}},
				DegradedVaults: []string{"stale-vault"},
			}, nil
		},
	}
	srv := newPublicTestServer(t, eng)

	stream := &mockActivateStream{ctx: context.Background()}
	err := srv.Activate(&pb.ActivateRequest{
		Context:      []string{"test"},
		Vaults:       []string{"proj-a", "agent-memory"},
		VaultWeights: []float64{0.67, 0.33},
	}, stream)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("sent %d responses, want 1", len(stream.sent))
	}
	if len(stream.sent[0].DegradedVaults) != 1 {
		t.Errorf("DegradedVaults not passed through, got %v", stream.sent[0].DegradedVaults)
	}
}

func TestActivate_MultiVault_VaultAndVaultsMutuallyExclusive(t *testing.T) {
	eng := &mockEngine{}
	srv := newPublicTestServer(t, eng)

	stream := &mockActivateStream{ctx: context.Background()}
	err := srv.Activate(&pb.ActivateRequest{
		Vault:  "default",
		Vaults: []string{"proj-a", "agent-memory"},
	}, stream)
	if err == nil {
		t.Fatal("expected error for vault+vaults mutual exclusivity")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

func TestActivate_MultiVault_ScopeViolationRejectedNoLeak(t *testing.T) {
	eng := &mockEngine{
		activateMultiFn: func(ctx context.Context, reqs []*pb.ActivateRequest, weights []float64) (*pb.ActivateResponse, error) {
			t.Fatal("engine.ActivateMulti must not be called when scope check fails")
			return nil, nil
		},
	}
	srv := newPublicTestServer(t, eng)

	ctx := context.WithValue(context.Background(), auth.ContextAPIKey, &auth.APIKey{
		Vaults: []string{"agent-memory", "proj-*"},
		Mode:   auth.ModeFull,
	})
	stream := &mockActivateStream{ctx: ctx}
	err := srv.Activate(&pb.ActivateRequest{
		Context: []string{"test"},
		Vaults:  []string{"agent-memory", "someone-elses-vault"},
	}, stream)
	if err == nil {
		t.Fatal("expected scope-violation error")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
	if containsAny(st.Message(), "agent-memory", "proj-*") {
		t.Errorf("error leaked scope contents: %q", st.Message())
	}
}

func TestActivate_MultiVault_AllInScopeSucceeds(t *testing.T) {
	eng := &mockEngine{
		activateMultiFn: func(ctx context.Context, reqs []*pb.ActivateRequest, weights []float64) (*pb.ActivateResponse, error) {
			return &pb.ActivateResponse{TotalFound: 0}, nil
		},
	}
	srv := newPublicTestServer(t, eng)

	ctx := context.WithValue(context.Background(), auth.ContextAPIKey, &auth.APIKey{
		Vaults: []string{"agent-memory", "proj-*"},
		Mode:   auth.ModeFull,
	})
	stream := &mockActivateStream{ctx: ctx}
	err := srv.Activate(&pb.ActivateRequest{
		Context: []string{"test"},
		Vaults:  []string{"agent-memory", "proj-muninndb-3f2a1b9c"},
	}, stream)
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("sent %d responses, want 1", len(stream.sent))
	}
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
