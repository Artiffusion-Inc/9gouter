package managedashboard

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakePricingRepo implements PricingService.Repo.
type fakePricingRepo struct {
	all       map[string]map[string]json.RawMessage
	allErr    error
	updateRet map[string]map[string]json.RawMessage
	updateErr error
	updateArg map[string]map[string]json.RawMessage
	resetArg  struct {
		provider, model string
	}
	resetRet map[string]map[string]json.RawMessage
	resetErr error
}

func (r *fakePricingRepo) GetAll(ctx context.Context) (map[string]map[string]json.RawMessage, error) {
	if r.allErr != nil {
		return nil, r.allErr
	}
	return r.all, nil
}

func (r *fakePricingRepo) Update(ctx context.Context, data map[string]map[string]json.RawMessage) (map[string]map[string]json.RawMessage, error) {
	r.updateArg = data
	if r.updateErr != nil {
		return nil, r.updateErr
	}
	return r.updateRet, nil
}

func (r *fakePricingRepo) Reset(ctx context.Context, provider, model string) (map[string]map[string]json.RawMessage, error) {
	r.resetArg.provider = provider
	r.resetArg.model = model
	if r.resetErr != nil {
		return nil, r.resetErr
	}
	return r.resetRet, nil
}

func TestPricingService_Get(t *testing.T) {
	t.Parallel()
	in := map[string]map[string]json.RawMessage{"openai": {"gpt-4": json.RawMessage(`{"input":1}`)}}
	s := &PricingService{Repo: &fakePricingRepo{all: in}}
	got, err := s.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("got %v", got)
	}
}

func TestPricingService_Get_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("fail")
	s := &PricingService{Repo: &fakePricingRepo{allErr: sentinel}}
	if _, err := s.Get(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestPricingService_Update(t *testing.T) {
	t.Parallel()
	in := map[string]map[string]json.RawMessage{"openai": {"gpt-4": json.RawMessage(`{}`)}}
	want := map[string]map[string]json.RawMessage{"openai": {"gpt-4": json.RawMessage(`{"input":2}`)}}
	repo := &fakePricingRepo{updateRet: want}
	s := &PricingService{Repo: repo}

	got, err := s.Update(context.Background(), in)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if !reflect.DeepEqual(repo.updateArg, in) {
		t.Errorf("Update arg = %v, want %v", repo.updateArg, in)
	}
}

func TestPricingService_Update_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("update fail")
	s := &PricingService{Repo: &fakePricingRepo{updateErr: sentinel}}
	if _, err := s.Update(context.Background(), nil); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestPricingService_Reset(t *testing.T) {
	t.Parallel()
	want := map[string]map[string]json.RawMessage{}
	repo := &fakePricingRepo{resetRet: want}
	s := &PricingService{Repo: repo}

	got, err := s.Reset(context.Background(), "openai", "gpt-4")
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if repo.resetArg.provider != "openai" || repo.resetArg.model != "gpt-4" {
		t.Errorf("reset args = %q/%q", repo.resetArg.provider, repo.resetArg.model)
	}
}

func TestPricingService_Reset_Error(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("reset fail")
	s := &PricingService{Repo: &fakePricingRepo{resetErr: sentinel}}
	if _, err := s.Reset(context.Background(), "p", "m"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestPricingService_Validate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    map[string]map[string]json.RawMessage
		wantErr string
	}{
		{
			name:    "empty body ok",
			body:    map[string]map[string]json.RawMessage{},
			wantErr: "",
		},
		{
			name: "valid fields float",
			body: map[string]map[string]json.RawMessage{
				"openai": {"gpt-4": json.RawMessage(`{"input":1.5,"output":2.0}`)},
			},
			wantErr: "",
		},
		{
			name: "valid fields string number",
			body: map[string]map[string]json.RawMessage{
				"openai": {"gpt-4": json.RawMessage(`{"input":"1.5"}`)},
			},
			wantErr: "",
		},
		{
			name: "valid fields json number",
			body: map[string]map[string]json.RawMessage{
				"openai": {"gpt-4": json.RawMessage(`{"input":1.5}`)},
			},
			wantErr: "",
		},
		{
			name: "nil models map",
			body: map[string]map[string]json.RawMessage{
				"openai": nil,
			},
			wantErr: "invalid pricing for provider: openai",
		},
		{
			name: "nil pricing raw",
			body: map[string]map[string]json.RawMessage{
				"openai": {"gpt-4": nil},
			},
			wantErr: "invalid pricing for model: openai/gpt-4",
		},
		{
			name: "invalid raw json",
			body: map[string]map[string]json.RawMessage{
				"openai": {"gpt-4": json.RawMessage(`{not json`)},
			},
			wantErr: "invalid pricing for model: openai/gpt-4",
		},
		{
			name: "invalid field name",
			body: map[string]map[string]json.RawMessage{
				"openai": {"gpt-4": json.RawMessage(`{"bogus":1}`)},
			},
			wantErr: "invalid pricing field: bogus for openai/gpt-4",
		},
		{
			name: "negative float",
			body: map[string]map[string]json.RawMessage{
				"openai": {"gpt-4": json.RawMessage(`{"input":-1}`)},
			},
			wantErr: "invalid pricing value for input in openai/gpt-4: must be non-negative number",
		},
		{
			name: "negative string number",
			body: map[string]map[string]json.RawMessage{
				"openai": {"gpt-4": json.RawMessage(`{"input":"-2.5"}`)},
			},
			wantErr: "invalid pricing value for input in openai/gpt-4: must be non-negative number",
		},
		{
			name: "non-numeric string",
			body: map[string]map[string]json.RawMessage{
				"openai": {"gpt-4": json.RawMessage(`{"input":"abc"}`)},
			},
			wantErr: "invalid pricing value for input in openai/gpt-4: must be non-negative number",
		},
		{
			name: "non-numeric type",
			body: map[string]map[string]json.RawMessage{
				"openai": {"gpt-4": json.RawMessage(`{"input":true}`)},
			},
			wantErr: "invalid pricing value for input in openai/gpt-4: must be non-negative number",
		},
		{
			name: "all valid fields",
			body: map[string]map[string]json.RawMessage{
				"openai": {"gpt-4": json.RawMessage(`{"input":1,"output":2,"cached":3,"reasoning":4,"cache_creation":5}`)},
			},
			wantErr: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &PricingService{}
			err := s.Validate(tc.body)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate: nil, want error %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate err = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestPricingService_Validate_NestedFieldErrorMessages(t *testing.T) {
	t.Parallel()
	// Verify specific provider/model appears in the error message.
	s := &PricingService{}
	err := s.Validate(map[string]map[string]json.RawMessage{
		"anthropic": {"claude-3": json.RawMessage(`{"invalid_field":1}`)},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "anthropic/claude-3") {
		t.Errorf("err = %q, want it to mention anthropic/claude-3", err.Error())
	}
}