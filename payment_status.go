package main

import (
	"context"
	"log/slog"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type pendingPayment struct {
	id     uuid.UUID
	bankID string
	total  float32
}

func (payments *orderPayments) runStatusSync(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	cursor := uuid.Nil
	for ctx.Err() == nil {
		next, err := payments.syncBatch(ctx, cursor)
		cursor = next
		if err != nil && ctx.Err() == nil {
			slog.Error("payment status sync failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (payments *orderPayments) syncBatch(ctx context.Context, after uuid.UUID) (uuid.UUID, error) {
	const batchSize = 32
	queryCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	rows, err := payments.db.Query(queryCtx, `SELECT id, payment_id, total_price FROM "order"
		WHERE payment_status = 'Processing' AND payment_id IS NOT NULL AND payment_id <> ''
			AND is_deleted = false AND id > $1 ORDER BY id LIMIT $2`, after, batchSize)
	if err != nil {
		return after, err
	}
	pending, err := pgx.CollectRows(rows, scanPendingPayment)
	if err != nil {
		return after, err
	}
	for _, payment := range pending {
		if ctx.Err() != nil {
			return after, ctx.Err()
		}
		if err := payments.syncPayment(ctx, payment); err != nil {
			slog.Error("could not persist bank payment status", "order", payment.id)
		}
		after = payment.id
	}
	if len(pending) < batchSize {
		return uuid.Nil, nil
	}
	return after, nil
}

func scanPendingPayment(row pgx.CollectableRow) (pendingPayment, error) {
	var payment pendingPayment
	err := row.Scan(&payment.id, &payment.bankID, &payment.total)
	return payment, err
}

func (payments *orderPayments) syncPayment(ctx context.Context, payment pendingPayment) error {
	var status struct {
		OrderStatus  *int16  `json:"orderStatus"`
		Amount       *uint64 `json:"amount"`
		Currency     *string `json:"currency"`
		ErrorCode    *string `json:"errorCode"`
		ErrorMessage *string `json:"errorMessage"`
	}
	err := payments.bankPost(ctx, payments.statusURL, url.Values{"orderId": {payment.bankID}}, &status)
	if err != nil || status.OrderStatus == nil || status.Amount == nil || status.Currency == nil ||
		status.ErrorCode == nil || status.ErrorMessage == nil {
		if ctx.Err() == nil {
			slog.Warn("bank status request unavailable", "order", payment.id)
		}
		return nil
	}
	if *status.ErrorCode != "0" {
		return nil
	}

	var next string
	switch *status.OrderStatus {
	case 2:
		expected, err := checkoutCents(payment.total)
		if err != nil || *status.Currency != bankCurrency || *status.Amount != uint64(expected) {
			slog.Error("bank amount or currency mismatch", "order", payment.id)
			return nil
		}
		next = "Completed"
	case 3, 6:
		next = "Failed"
	default:
		return nil
	}

	writeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return pgx.BeginTxFunc(writeCtx, payments.db, pgx.TxOptions{AccessMode: pgx.ReadWrite}, func(tx pgx.Tx) error {
		_, err := tx.Exec(writeCtx, `UPDATE "order" SET payment_status = $3::text::payment_status, updated_at = now()
			WHERE id = $1 AND payment_status = 'Processing' AND payment_id = $2
				AND is_deleted = false AND ($3 <> 'Completed' OR total_price = $4)`,
			payment.id, payment.bankID, next, payment.total)
		return err
	})
}
