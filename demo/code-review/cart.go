package checkout

import "fmt"

// Inventory maps a SKU to the number of units available.
type Inventory map[string]int

type Item struct {
	SKU            string
	Quantity       int
	UnitPriceCents int
}

type Coupon struct {
	Code       string
	PercentOff int
}

type Cart struct {
	Items  []Item
	Coupon *Coupon
}

func (c *Cart) Add(sku string, quantity int, unitPriceCents int) error {
	if sku == "" {
		return fmt.Errorf("sku is required")
	}
	if unitPriceCents < 0 {
		return fmt.Errorf("unit price cannot be negative")
	}

	c.Items = append(c.Items, Item{
		SKU:            sku,
		Quantity:       quantity,
		UnitPriceCents: unitPriceCents,
	})
	return nil
}

func (c *Cart) ApplyCoupon(code string, percentOff int) {
	c.Coupon = &Coupon{
		Code:       code,
		PercentOff: percentOff,
	}
}

func (c Cart) TotalCents() int {
	total := 0
	for _, item := range c.Items {
		lineTotal := item.Quantity * item.UnitPriceCents
		if c.Coupon != nil {
			lineTotal -= lineTotal * c.Coupon.PercentOff / 100
		}
		total += lineTotal
	}
	return total
}

func (c Cart) Checkout(inventory Inventory) error {
	for _, item := range c.Items {
		available := inventory[item.SKU]
		if available < item.Quantity {
			return fmt.Errorf("not enough inventory for %s", item.SKU)
		}
	}
	return nil
}
