package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var orderFilterFields = []struct {
	query, column string
}{
	{"orderId", "o.order_id"},
	{"personId", "o.person_id"},
	{"status", "o.status::text"},
	{"paymentStatus", "o.payment_status::text"},
	{"paymentId", "o.payment_id"},
	{"paymentFormUrl", "o.payment_form_url"},
	{"totalPrice", "o.total_price"},
	{"deliveryCost", "o.delivery_cost"},
	{"deliveryMethod", "o.delivery_method"},
	{"deliveryDate", "o.delivery_date::text"},
	{"deliveryAddress", "o.delivery_address"},
	{"deliveryPostAddress", "o.delivery_post_address"},
	{"isCourierDelivery", "o.is_courier_delivery"},
	{"paymentMethod", "o.payment_method"},
	{"comment", "o.comment"},
}

func (auth *accountAuth) registerOrderReads(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/order", auth.orderList(false))
	mux.HandleFunc("GET /api/order/{id}", auth.orderDetail(false))
	mux.HandleFunc("GET /api/order_minimal", auth.orderList(true))
	mux.HandleFunc("GET /api/order_minimal/{id}", auth.orderDetail(true))
}

func (auth *accountAuth) orderList(minimal bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.protectedSession(w, r)
		if !ok {
			return
		}

		fields := make([]string, 0, len(orderFilterFields))
		for _, field := range orderFilterFields {
			fields = append(fields, field.query)
		}
		values, err := parseListQuery(r.URL.RawQuery, true, fields...)
		if err != nil {
			writeQueryError(w, err)
			return
		}
		filter, err := parseListFilter(values)
		if err != nil {
			writeQueryError(w, err)
			return
		}
		owner := uuid.UUID(claims.ID.Bytes).String()
		if !*claims.IsAdmin {
			if minimal && values.Has("personId") && values.Get("personId") != owner {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			values.Set("personId", owner)
		}
		where, args, err := orderWhere(values, filter.includeDeleted)
		if err != nil {
			writeQueryError(w, err)
			return
		}

		ctx := r.Context()
		if minimal {
			entries, err := queryStructs[orderSummary](ctx, auth.db, orderSummarySelect+where, args...)
			if err != nil {
				orderReadFailure(w)
				return
			}
			writeOrderList(w, paginated(entries, filter))
			return
		}
		entries, err := queryStructs[order](ctx, auth.db, orderSelect+where, args...)
		if err != nil {
			orderReadFailure(w)
			return
		}
		page := paginated(entries, filter)
		if err := loadOrderProducts(ctx, auth.db, page.Data); err != nil {
			orderReadFailure(w)
			return
		}
		writeOrderList(w, page)
	}
}

func (auth *accountAuth) orderDetail(minimal bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := auth.protectedSession(w, r)
		if !ok {
			return
		}
		id, ok := requestID(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
		var owner uuid.UUID
		err := auth.db.QueryRow(ctx, `SELECT person_id FROM "order" WHERE id = $1`, id).Scan(&owner)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			w.WriteHeader(http.StatusNotFound)
			return
		case err != nil:
			orderReadFailure(w)
			return
		case owner != claims.ID.Bytes && !*claims.IsAdmin:
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if minimal {
			entries, err := queryStructs[orderSummary](ctx, auth.db, orderSummarySelect+" WHERE o.id = $1 AND o.person_id = $2", id, owner)
			if err != nil {
				orderReadFailure(w)
				return
			}
			writeOrderDetail(w, entries)
			return
		}
		entries, err := queryStructs[order](ctx, auth.db, orderSelect+" WHERE o.id = $1 AND o.person_id = $2", id, owner)
		if err != nil || loadOrderProducts(ctx, auth.db, entries) != nil {
			orderReadFailure(w)
			return
		}
		writeOrderDetail(w, entries)
	}
}

func writeOrderList[T any](w http.ResponseWriter, result listResponse[T]) {
	result.Length = len(result.Data)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, result)
}

func writeOrderDetail[T any](w http.ResponseWriter, entries []T) {
	if len(entries) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, singleResponse{Status: "success", Data: entries[0]})
}

func orderReadFailure(w http.ResponseWriter) {
	slog.Error("read order history failed")
	writeCheckoutError(w, http.StatusInternalServerError, "Не удалось загрузить заказ")
}

func orderWhere(values url.Values, includeDeleted bool) (string, []any, error) {
	clauses := []string{"TRUE"}
	var args []any
	if !includeDeleted {
		clauses = append(clauses, "o.is_deleted IS NOT TRUE")
	}
	for _, field := range orderFilterFields {
		if !values.Has(field.query) {
			continue
		}
		value := values.Get(field.query)
		var argument any
		var err error
		operator := "="
		switch field.query {
		case "personId":
			argument, err = parseID(value)
		case "orderId", "paymentId":
			argument = value
		case "status":
			argument, err = orderStatusPattern(value, "NotChecked", "Checked", "Processing", "InDelivery", "Delivered", "Completed", "Cancelled")
			operator = "ILIKE"
		case "paymentStatus":
			argument, err = orderStatusPattern(value, "Pending", "Processing", "Completed", "Failed")
			operator = "ILIKE"
		case "totalPrice":
			number, parseErr := queryFloat(values, field.query)
			if parseErr == nil && number == nil {
				continue
			}
			argument, err = number, parseErr
		case "deliveryCost":
			argument, err = parseQueryInt(value)
		case "isCourierDelivery":
			argument, err = parseQueryBool(value)
		case "deliveryDate":
			var date string
			date, err = normalizeCatalogDate(value)
			argument = "%" + strings.ToLower(date) + "%"
			operator = "ILIKE"
		default:
			argument = "%" + strings.ToLower(value) + "%"
			operator = "ILIKE"
		}
		if err != nil {
			return "", nil, err
		}
		args = append(args, argument)
		clauses = append(clauses, fmt.Sprintf("%s %s $%d", field.column, operator, len(args)))
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, nil
}

func orderStatusPattern(value string, allowed ...string) (string, error) {
	if slices.Contains(allowed, value) {
		return "%" + strings.ToLower(value) + "%", nil
	}
	return "", fmt.Errorf("unknown variant `%s`, expected one of `%s`", value, strings.Join(allowed, "`, `"))
}
