package model

import "testing"

func TestNewRejectsUnknownProvider(t *testing.T) {
	_, err := New(Config{Provider: "not-real"})
	if err == nil {
		t.Fatal("expected an error for an unknown provider")
	}
}

func TestNewSupportsConfiguredProviders(t *testing.T) {
	for _, provider := range []Provider{
		"",
		ProviderAnthropic,
		ProviderOpenAI,
		ProviderGoogle,
	} {
		t.Run(string(provider), func(t *testing.T) {
			got, err := New(Config{Provider: provider})
			if err != nil {
				t.Fatal(err)
			}
			if got == nil {
				t.Fatal("expected a model")
			}
		})
	}
}

func TestHeadersWithAuthorization(t *testing.T) {
	t.Run("derives bearer token from API key", func(t *testing.T) {
		headers := headersWithAuthorization(nil, "secret")
		if got := headers["Authorization"]; got != "Bearer secret" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer secret")
		}
	})

	t.Run("preserves explicit authorization", func(t *testing.T) {
		headers := headersWithAuthorization(
			map[string]string{"Authorization": "Custom secret"},
			"api-key",
		)
		if got := headers["Authorization"]; got != "Custom secret" {
			t.Fatalf("Authorization = %q, want %q", got, "Custom secret")
		}
	})

	t.Run("does not mutate caller headers", func(t *testing.T) {
		original := map[string]string{"X-Custom": "value"}
		headersWithAuthorization(original, "secret")
		if _, exists := original["Authorization"]; exists {
			t.Fatal("caller headers were mutated")
		}
	})
}
