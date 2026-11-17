package main

import (
	jsonv2 "encoding/json/v2"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const rustCheckoutRequest = `{
  "details": {"offerConsent":true, "firstName":"  Тест  ", "secondName":" Покупатель ", "phone":"+375 (29) 000-00-11", "email":" Demo@Example.invalid  ", "deliveryMethod":"post", "paymentMethod":"cash", "isCourierDelivery":true, "address":{"city":" Минск ","street":" Тестовая ","house":" 10 ","building":" 2 ","apartment":" 3 "}, "postPlacementId":123, "deliveryPostAddress":"  Проверочное отделение  ", "comment":"Проверка <>&/\u2028\u2029\t\u000b"},
  "items":[{"variantId":"00000000-0000-0000-0000-000000000602","quantity":1},{"variantId":"00000000-0000-0000-0000-000000000601","quantity":2},{"variantId":"00000000-0000-0000-0000-000000000602","quantity":3}],
  "checkoutKey":"00000000-0000-0000-0000-000000000901",
  "cartId":"00000000-0000-0000-0000-000000000900",
  "expectedRevision":3,
  "expectedTotalCents":24999,
  "promoCode":" SAVE "
}`

func TestCheckoutRustFingerprint(t *testing.T) {
	request := checkoutRequestFixture(t)
	address, fingerprint, err := normalizeCheckout(&request)
	if err != nil {
		t.Fatal(err)
	}
	if address == nil || *address != "Минск, Тестовая, д. 10, корпус 2, кв. 3" {
		t.Fatalf("formatted address = %v", address)
	}
	// Calculated with the Rust CheckoutRequest schema and serde_json from Cargo.lock.
	const expected = "6a3836a093355ca2a39e8c1d09784e70b0690279867e89aca93f972ed2a71cae"
	if fingerprint != expected {
		t.Fatalf("fingerprint = %s, want %s", fingerprint, expected)
	}
	if request.Details.Phone == nil || *request.Details.Phone != "+375290000011" ||
		request.Details.Email == nil || *request.Details.Email != "Demo@Example.invalid" {
		t.Fatalf("contact details did not normalize")
	}
	if len(request.Items) != 2 || request.Items[0].Quantity != 2 || request.Items[1].Quantity != 4 ||
		request.Items[0].VariantID.String() != "00000000-0000-0000-0000-000000000601" {
		t.Fatalf("items = %v", request.Items)
	}
}

func TestCheckoutInputValidation(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		mutate     func(*checkoutRequest)
	}{
		{"empty items", "Добавьте товары в корзину", func(r *checkoutRequest) { r.Items = []cartItem{} }},
		{"zero key", "Добавьте товары в корзину", func(r *checkoutRequest) { r.CheckoutKey = uuid.Nil }},
		{"invalid name", "Укажите имя и фамилию (до 100 символов)", func(r *checkoutRequest) { r.Details.FirstName = "Те\x00ст" }},
		{"invalid email", "Проверьте адрес электронной почты", func(r *checkoutRequest) { r.Details.Email = checkoutText("test@@example.invalid") }},
		{"offer", "Подтвердите согласие с офертой", func(r *checkoutRequest) { r.Details.OfferConsent = false }},
		{"payment", "Выберите способ оплаты", func(r *checkoutRequest) { r.Details.PaymentMethod = "invoice" }},
		{"delivery", "Invalid delivery method", func(r *checkoutRequest) { r.Details.DeliveryMethod = "courier" }},
		{"long comment", "Комментарий слишком длинный", func(r *checkoutRequest) { r.Details.Comment = checkoutText(strings.Repeat("я", 2001)) }},
		{"missing branch", "Выберите отделение Европочты из списка", func(r *checkoutRequest) { r.Details.PostPlacementID = nil }},
		{"blank branch address", "Выберите отделение Европочты из списка", func(r *checkoutRequest) { r.Details.DeliveryPostAddress = checkoutText(" ") }},
		{"missing courier address", "Укажите адрес доставки", func(r *checkoutRequest) { r.Details.Address = nil }},
		{"incomplete address", "Заполните город, улицу и дом", func(r *checkoutRequest) { r.Details.Address.City = "  " }},
		{"long optional address field", "Проверьте адрес доставки", func(r *checkoutRequest) { r.Details.Address.Apartment = strings.Repeat("я", 41) }},
		{"invalid phone characters", "Введите белорусский номер телефона", func(r *checkoutRequest) { r.Details.Phone = checkoutText("+375	290000011") }},
		{"invalid phone prefix", "Введите номер в формате +375 XX XXX-XX-XX", func(r *checkoutRequest) { r.Details.Phone = checkoutText("+370290000011") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := checkoutRequestFixture(t)
			tc.mutate(&request)
			_, _, err := normalizeCheckout(&request)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCheckoutOptionalFields(t *testing.T) {
	const raw = `{"cartId":"00000000-0000-0000-0000-000000000000","checkoutKey":"00000000-0000-0000-0000-000000000901","expectedRevision":0,"items":[{"variantId":"00000000-0000-0000-0000-000000000601","quantity":1}],"details":{"firstName":"Тест","secondName":"Гость","deliveryMethod":"pickup","paymentMethod":"cash","offerConsent":true},"expectedTotalCents":0}`
	var request checkoutRequest
	if err := jsonv2.Unmarshal([]byte(raw), &request); err != nil || !request.valid() {
		t.Fatalf("decode: %v", err)
	}
	address, fingerprint, err := normalizeCheckout(&request)
	if err != nil || address != nil || request.PromoCode != nil || request.Details.Phone != nil ||
		request.Details.Email != nil || request.Details.IsCourierDelivery {
		t.Fatalf("unexpected defaults: %v", err)
	}
	const expected = "a585f75a19bac276b9fb40a84cbb60df5a26773b2988ad8ced234de1125e6bdd"
	if fingerprint != expected {
		t.Fatalf("fingerprint with defaults = %s, want %s", fingerprint, expected)
	}
	for _, value := range []string{
		strings.ReplaceAll(raw, `"cartId":"00000000-0000-0000-0000-000000000000",`, ""),
		strings.ReplaceAll(raw, `"checkoutKey":"00000000-0000-0000-0000-000000000901"`, `"checkoutKey":null`),
		strings.ReplaceAll(raw, `"expectedTotalCents":0`, `"expectedTotalCents":null`),
		strings.ReplaceAll(raw, `"offerConsent":true`, `"isCourierDelivery":null,"offerConsent":true`),
		strings.ReplaceAll(raw, `"offerConsent":true`, `"address":{"city":null},"offerConsent":true`),
		strings.ReplaceAll(raw, `,"offerConsent":true`, ""),
	} {
		request = checkoutRequest{}
		if err := jsonv2.Unmarshal([]byte(value), &request); err == nil && request.valid() {
			t.Fatalf("invalid request was accepted: %s", value)
		}
	}
}

func checkoutRequestFixture(t *testing.T) checkoutRequest {
	t.Helper()
	var request checkoutRequest
	if err := jsonv2.Unmarshal([]byte(rustCheckoutRequest), &request); err != nil || !request.valid() {
		t.Fatalf("decode: %v", err)
	}
	return request
}

func checkoutText(value string) *string {
	return &value
}
