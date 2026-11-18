package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

type checkoutRequest struct {
	CartID             uuid.UUID        `json:"cartId"`
	ExpectedRevision   *int64           `json:"expectedRevision"`
	CheckoutKey        uuid.UUID        `json:"checkoutKey"`
	Items              []cartItem       `json:"items"`
	PromoCode          *string          `json:"promoCode"`
	Details            *checkoutDetails `json:"details"`
	ExpectedTotalCents *int64           `json:"expectedTotalCents"`
}

type checkoutDetails struct {
	Phone               *string          `json:"phone"`
	FirstName           string           `json:"firstName"`
	SecondName          string           `json:"secondName"`
	Email               *string          `json:"email"`
	DeliveryMethod      string           `json:"deliveryMethod"`
	PaymentMethod       string           `json:"paymentMethod"`
	IsCourierDelivery   bool             `json:"isCourierDelivery"`
	Address             *deliveryAddress `json:"address"`
	PostPlacementID     *int32           `json:"postPlacementId"`
	DeliveryPostAddress *string          `json:"deliveryPostAddress"`
	Comment             *string          `json:"comment"`
	OfferConsent        bool             `json:"offerConsent"`
}

type deliveryAddress struct {
	City       string `json:"city"`
	Street     string `json:"street"`
	House      string `json:"house"`
	Building   string `json:"building"`
	Apartment  string `json:"apartment"`
	PostalCode string `json:"postalCode"`
}

func (request *checkoutRequest) UnmarshalJSONFrom(decoder *jsontext.Decoder) error {
	type requestFields checkoutRequest
	wire := struct {
		*requestFields
		CartID      *uuid.UUID `json:"cartId"`
		CheckoutKey *uuid.UUID `json:"checkoutKey"`
	}{requestFields: (*requestFields)(request)}
	if err := jsonv2.UnmarshalDecode(decoder, &wire); err != nil {
		return err
	}
	if wire.CartID == nil || wire.CheckoutKey == nil {
		return quoteInputError("Некорректный заказ")
	}
	request.CartID, request.CheckoutKey = *wire.CartID, *wire.CheckoutKey
	return nil
}

func (details *checkoutDetails) UnmarshalJSONFrom(decoder *jsontext.Decoder) error {
	type detailsFields checkoutDetails
	courier := false
	wire := struct {
		*detailsFields
		FirstName         *string `json:"firstName"`
		SecondName        *string `json:"secondName"`
		DeliveryMethod    *string `json:"deliveryMethod"`
		PaymentMethod     *string `json:"paymentMethod"`
		IsCourierDelivery *bool   `json:"isCourierDelivery"`
		OfferConsent      *bool   `json:"offerConsent"`
	}{detailsFields: (*detailsFields)(details), IsCourierDelivery: &courier}
	if err := jsonv2.UnmarshalDecode(decoder, &wire); err != nil {
		return err
	}
	if wire.FirstName == nil || wire.SecondName == nil || wire.DeliveryMethod == nil ||
		wire.PaymentMethod == nil || wire.IsCourierDelivery == nil || wire.OfferConsent == nil {
		return quoteInputError("Некорректные контакты")
	}
	details.FirstName, details.SecondName = *wire.FirstName, *wire.SecondName
	details.DeliveryMethod, details.PaymentMethod = *wire.DeliveryMethod, *wire.PaymentMethod
	details.IsCourierDelivery, details.OfferConsent = *wire.IsCourierDelivery, *wire.OfferConsent
	return nil
}

func (address *deliveryAddress) UnmarshalJSONFrom(decoder *jsontext.Decoder) error {
	type addressFields deliveryAddress
	return jsonv2.UnmarshalDecode(decoder, (*addressFields)(address),
		jsonv2.WithUnmarshalers(jsonv2.UnmarshalFromFunc(rejectNullAddressField)))
}

func rejectNullAddressField(decoder *jsontext.Decoder, _ *string) error {
	if decoder.PeekKind() == 'n' {
		return quoteInputError("Проверьте адрес доставки")
	}
	return errors.ErrUnsupported
}

func (request checkoutRequest) valid() bool {
	return request.ExpectedRevision != nil && request.Items != nil && request.Details != nil && request.ExpectedTotalCents != nil
}

func normalizeCheckout(request *checkoutRequest) (*string, string, error) {
	if !request.valid() {
		return nil, "", quoteInputError("Некорректный заказ")
	}
	items, err := normalizeCartItems(request.Items)
	if err != nil {
		return nil, "", err
	}
	if len(items) == 0 || request.CheckoutKey == uuid.Nil {
		return nil, "", quoteInputError("Добавьте товары в корзину")
	}
	request.Items = items
	address, err := normalizeCheckoutDetails(request.Details)
	if err != nil {
		return nil, "", err
	}
	if request.Details.Phone != nil {
		phone, err := normalizeCheckoutPhone(*request.Details.Phone)
		if err != nil {
			return nil, "", err
		}
		request.Details.Phone = &phone
	}
	request.PromoCode = trimCheckoutValue(request.PromoCode)

	// Rust hashes the normalized request in struct field order, including nulls and defaults.
	data, err := jsonv2.Marshal(request)
	if err != nil {
		return nil, "", quoteInputError("Некорректный заказ")
	}
	sum := sha256.Sum256(data)
	return address, hex.EncodeToString(sum[:]), nil
}

func validateCheckoutContact(first, second string, email *string) (string, string, *string, error) {
	first, second = strings.TrimSpace(first), strings.TrimSpace(second)
	if first == "" || !validCheckoutText(first, 100) || second == "" || !validCheckoutText(second, 100) {
		return "", "", nil, quoteInputError("Укажите имя и фамилию (до 100 символов)")
	}

	email = trimCheckoutValue(email)
	if email != nil {
		local, domain, found := strings.Cut(*email, "@")
		if !found || len(*email) > 254 || strings.ContainsFunc(*email, invalidCheckoutEmailRune) ||
			local == "" || strings.Contains(domain, "@") || !strings.Contains(domain, ".") ||
			strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
			return "", "", nil, quoteInputError("Проверьте адрес электронной почты")
		}
	}
	return first, second, email, nil
}

func normalizeCheckoutDetails(details *checkoutDetails) (*string, error) {
	first, second, email, err := validateCheckoutContact(details.FirstName, details.SecondName, details.Email)
	if err != nil {
		return nil, err
	}
	details.FirstName, details.SecondName, details.Email = first, second, email

	if !details.OfferConsent {
		return nil, quoteInputError("Подтвердите согласие с офертой")
	}
	switch details.PaymentMethod {
	case "cash", "card", "online":
	default:
		return nil, quoteInputError("Выберите способ оплаты")
	}
	if _, err := quoteDeliveryCost(details.DeliveryMethod, details.IsCourierDelivery, 0); err != nil {
		return nil, err
	}
	if details.Comment != nil && utf8.RuneCountInString(*details.Comment) > 2000 {
		return nil, quoteInputError("Комментарий слишком длинный")
	}
	if details.DeliveryMethod == "post" && (details.PostPlacementID == nil || *details.PostPlacementID <= 0 ||
		details.DeliveryPostAddress == nil || strings.TrimSpace(*details.DeliveryPostAddress) == "" ||
		utf8.RuneCountInString(*details.DeliveryPostAddress) > 500) {
		return nil, quoteInputError("Выберите отделение Европочты из списка")
	}
	if details.DeliveryMethod == "standart" || (details.DeliveryMethod == "post" && details.IsCourierDelivery) {
		return formatCheckoutAddress(details.Address)
	}
	return nil, nil
}

func formatCheckoutAddress(address *deliveryAddress) (*string, error) {
	if address == nil {
		return nil, quoteInputError("Укажите адрес доставки")
	}
	for _, field := range []*string{&address.City, &address.Street, &address.House} {
		*field = strings.TrimSpace(*field)
		if *field == "" || !validCheckoutText(*field, 200) {
			return nil, quoteInputError("Заполните город, улицу и дом")
		}
	}
	for _, field := range []string{address.Building, address.Apartment, address.PostalCode} {
		if !validCheckoutText(field, 40) {
			return nil, quoteInputError("Проверьте адрес доставки")
		}
	}
	text := address.City + ", " + address.Street + ", д. " + address.House
	if building := strings.TrimSpace(address.Building); building != "" {
		text += ", корпус " + building
	}
	if apartment := strings.TrimSpace(address.Apartment); apartment != "" {
		text += ", кв. " + apartment
	}
	return &text, nil
}

func normalizeCheckoutPhone(phone string) (string, error) {
	var digits strings.Builder
	for _, char := range phone {
		switch {
		case char >= '0' && char <= '9':
			digits.WriteByte(byte(char))
		case char == '+' || char == ' ' || char == '-' || char == '(' || char == ')':
		default:
			return "", quoteInputError("Введите белорусский номер телефона")
		}
	}
	if digits.Len() != 12 || !strings.HasPrefix(digits.String(), "375") {
		return "", quoteInputError("Введите номер в формате +375 XX XXX-XX-XX")
	}
	return "+" + digits.String(), nil
}

func trimCheckoutValue(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func validCheckoutText(value string, limit int) bool {
	return utf8.RuneCountInString(value) <= limit && !strings.ContainsFunc(value, unicode.IsControl)
}

func invalidCheckoutEmailRune(char rune) bool {
	return unicode.IsSpace(char) || unicode.IsControl(char)
}
