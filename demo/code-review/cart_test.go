package checkout

import "testing"

func TestTotalCentsAppliesCoupon(t *testing.T) {
	var cart Cart
	if err := cart.Add("coffee", 2, 500); err != nil {
		t.Fatal(err)
	}
	cart.ApplyCoupon("SAVE10", 10)

	if got, want := cart.TotalCents(), 900; got != want {
		t.Fatalf("TotalCents() = %d, want %d", got, want)
	}
}

func TestAddRejectsNegativeQuantities(t *testing.T) {
	var cart Cart

	if err := cart.Add("coffee", -2, 500); err == nil {
		t.Fatal("expected negative quantity to be rejected")
	}
}

func TestCheckoutRejectsMissingInventory(t *testing.T) {
	var cart Cart
	if err := cart.Add("coffee", 3, 500); err != nil {
		t.Fatal(err)
	}

	if err := cart.Checkout(Inventory{"coffee": 2}); err == nil {
		t.Fatal("expected checkout to reject unavailable inventory")
	}
}
