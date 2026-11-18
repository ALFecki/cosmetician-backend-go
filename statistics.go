package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type statisticsFilter struct {
	Brands       []uuid.UUID
	Categories   []uuid.UUID
	Start        *time.Time
	EndExclusive *time.Time
	Weight       float64
}

type statisticsBrand struct {
	BrandID   uuid.UUID `json:"brandId"`
	BrandName string    `json:"brandName"`
}

type brandProfitability struct {
	statisticsBrand
	ProfitabilityPercentage float64 `json:"profitabilityPercentage"`
	TotalRevenue            float64 `json:"totalRevenue"`
	TotalCost               float64 `json:"totalCost"`
	TotalProfit             float64 `json:"totalProfit"`
	UnitsSold               int64   `json:"unitsSold"`
}

type weightedROI struct {
	statisticsBrand
	WeightedROI             float64 `json:"weightedROI"`
	TotalProfit             float64 `json:"totalProfit"`
	TotalInvestment         float64 `json:"totalInvestment"`
	SignificanceCoefficient float64 `json:"significanceCoefficient"`
}

type inventoryTurnover struct {
	statisticsBrand
	TurnoverRatio         float64 `json:"turnoverRatio"`
	CostOfGoodsSold       float64 `json:"costOfGoodsSold"`
	AverageInventoryValue float64 `json:"averageInventoryValue"`
	TotalUnitsSold        int64   `json:"totalUnitsSold"`
	AverageInventoryUnits float64 `json:"averageInventoryUnits"`
}

type commerceStatistics struct {
	BrandProfitability []brandProfitability `json:"brandProfitability"`
	WeightedROI        []weightedROI        `json:"weightedROI"`
	InventoryTurnover  []inventoryTurnover  `json:"inventoryTurnover"`
}

func (auth *accountAuth) registerStatistics(mux *http.ServeMux) {
	for _, name := range []string{"summary", "brand-profitability", "weighted-roi", "inventory-turnover"} {
		mux.HandleFunc("GET /api/statistics/"+name, auth.requireCommerceAdmin(auth.serveStatistics(name)))
	}
}

func (auth *accountAuth) serveStatistics(report string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter, err := parseStatisticsFilter(r.URL.RawQuery)
		if err != nil {
			writeQueryError(w, err)
			return
		}
		result, err := loadStatistics(r.Context(), auth.db, filter)
		if err != nil {
			databaseFailure(w, "read sales statistics", err)
			return
		}
		switch report {
		case "brand-profitability":
			writeJSON(w, result.BrandProfitability)
		case "weighted-roi":
			writeJSON(w, result.WeightedROI)
		case "inventory-turnover":
			writeJSON(w, result.InventoryTurnover)
		default:
			writeJSON(w, singleResponse{Status: "success", Data: result})
		}
	}
}

func parseStatisticsFilter(raw string) (statisticsFilter, error) {
	filter := statisticsFilter{Weight: 1}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return filter, errors.New("некорректные параметры статистики")
	}
	filter.Brands, err = readStatisticsIDs(values, "brandId")
	if err != nil {
		return filter, err
	}
	filter.Categories, err = readStatisticsIDs(values, "categoryId")
	if err != nil {
		return filter, err
	}
	if start := values.Get("startDate"); start != "" {
		parsed, err := time.Parse(time.DateOnly, start)
		if err != nil {
			return filter, errors.New("некорректная дата начала")
		}
		filter.Start = &parsed
	}
	if end := values.Get("endDate"); end != "" {
		parsed, err := time.Parse(time.DateOnly, end)
		if err != nil {
			return filter, errors.New("некорректная дата окончания")
		}
		nextDay := parsed.AddDate(0, 0, 1)
		if filter.Start != nil && !nextDay.After(*filter.Start) {
			return filter, errors.New("дата окончания раньше даты начала")
		}
		filter.EndExclusive = &nextDay
	}
	if rawWeight := values.Get("significanceCoefficient"); rawWeight != "" {
		value, err := strconv.ParseFloat(rawWeight, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1_000_000 {
			return filter, errors.New("некорректный коэффициент значимости")
		}
		filter.Weight = value
	}
	return filter, nil
}

func readStatisticsIDs(values url.Values, key string) ([]uuid.UUID, error) {
	var result []uuid.UUID
	for _, value := range append(slices.Clone(values[key]), values[key+"[]"]...) {
		for identifier := range strings.SplitSeq(value, ",") {
			id, err := uuid.Parse(identifier)
			if err != nil || id == uuid.Nil || len(result) == 100 {
				return nil, errors.New("некорректный список фильтров статистики")
			}
			result = append(result, id)
		}
	}
	return result, nil
}

func loadStatistics(ctx context.Context, db database, filter statisticsFilter) (commerceStatistics, error) {
	rows, err := queryStructs[struct {
		statisticsBrand
		Revenue        float64
		Cost           float64
		Units          int64
		InventoryUnits float64
	}](ctx, db, `SELECT b.id, b.title, SUM(v.price::double precision * op.quantity),
		SUM(v.cost_price::double precision * op.quantity), SUM(op.quantity),
		AVG(op.quantity * 1.5)::double precision
		FROM brand b JOIN product p ON p.brand_id = b.id JOIN product_variant v ON v.product_id = p.id
		JOIN order_product op ON op.variant_id = v.id JOIN "order" o ON o.id = op.order_id
		WHERE o.status = 'Completed' AND ($1::uuid[] IS NULL OR b.id = ANY($1))
		AND ($2::uuid[] IS NULL OR EXISTS(SELECT 1 FROM product_category pc WHERE pc.product_id = p.id AND pc.category_id = ANY($2)))
		AND ($3::timestamptz IS NULL OR o.created_at >= $3) AND ($4::timestamptz IS NULL OR o.created_at < $4)
		GROUP BY b.id, b.title ORDER BY b.id`, filter.Brands, filter.Categories, filter.Start, filter.EndExclusive)
	result := commerceStatistics{
		BrandProfitability: make([]brandProfitability, 0, len(rows)),
		WeightedROI:        make([]weightedROI, 0, len(rows)),
		InventoryTurnover:  make([]inventoryTurnover, 0, len(rows)),
	}
	if err != nil {
		return result, err
	}
	for _, row := range rows {
		profit := row.Revenue - row.Cost
		weightedProfit, investment := profit*filter.Weight, row.Cost*filter.Weight
		inventoryValue := 1.5 * row.Cost
		result.BrandProfitability = append(result.BrandProfitability, brandProfitability{
			row.statisticsBrand, 100 * ratioOf(profit, row.Revenue), row.Revenue, row.Cost, profit, row.Units,
		})
		result.WeightedROI = append(result.WeightedROI, weightedROI{
			row.statisticsBrand, ratioOf(weightedProfit, investment), weightedProfit, investment, filter.Weight,
		})
		result.InventoryTurnover = append(result.InventoryTurnover, inventoryTurnover{
			row.statisticsBrand, ratioOf(row.Cost, inventoryValue), row.Cost, inventoryValue, row.Units, row.InventoryUnits,
		})
	}
	slices.SortStableFunc(result.BrandProfitability, func(a, b brandProfitability) int { return descendingMetric(a.TotalProfit, b.TotalProfit) })
	slices.SortStableFunc(result.WeightedROI, func(a, b weightedROI) int { return descendingMetric(a.TotalProfit, b.TotalProfit) })
	slices.SortStableFunc(result.InventoryTurnover, func(a, b inventoryTurnover) int { return descendingMetric(a.CostOfGoodsSold, b.CostOfGoodsSold) })
	return result, nil
}

func descendingMetric(a, b float64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	default:
		return 0
	}
}

func ratioOf(amount, base float64) float64 {
	if base <= 0 {
		return 0
	}
	return amount / base
}
