package marketplace

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/shopspring/decimal"
	"marketplace/catalog-service/internal/api"
)

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func orderByID(ctx context.Context, q querier, id int64, lock bool) (api.OrderResponse, error) {
	var order api.OrderResponse
	query := `SELECT id, user_id, status, promo_code_id, total_amount::text, discount_amount::text, created_at, updated_at FROM orders WHERE id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	err := q.QueryRowContext(ctx, query, id).Scan(&order.Id, &order.UserId, &order.Status, &order.PromoCodeId, &order.TotalAmount, &order.DiscountAmount, &order.CreatedAt, &order.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = fail(404, "ORDER_NOT_FOUND", "Заказ не найден")
	}
	if err != nil {
		return order, err
	}
	order.Items = []api.OrderItemResponse{}
	rows, err := q.QueryContext(ctx, `SELECT id, product_id, quantity, price_at_order::text FROM order_items WHERE order_id=$1 ORDER BY id`, id)
	if err != nil {
		return order, err
	}
	defer rows.Close()
	for rows.Next() {
		var item api.OrderItemResponse
		if err := rows.Scan(&item.Id, &item.ProductId, &item.Quantity, &item.PriceAtOrder); err != nil {
			return order, err
		}
		order.Items = append(order.Items, item)
	}
	return order, rows.Err()
}

func ownsOrder(r *http.Request, order api.OrderResponse) error {
	if current(r).Role != "ADMIN" && order.UserId != current(r).ID {
		return fail(403, "ORDER_OWNERSHIP_VIOLATION", "Заказ принадлежит другому пользователю")
	}
	return nil
}

func lockUser(ctx context.Context, tx *sql.Tx, userID int64) error {
	return tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&userID)
}

func (s *Server) checkInterval(ctx context.Context, tx *sql.Tx, userID int64, operation string) error {
	var last sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT max(created_at) FROM user_operations WHERE user_id=$1 AND operation_type=$2`, userID, operation).Scan(&last)
	if err != nil {
		return err
	}
	if last.Valid && time.Since(last.Time) < s.interval {
		return fail(429, "ORDER_LIMIT_EXCEEDED", "Слишком частые операции с заказом")
	}
	return nil
}

func recordOperation(ctx context.Context, tx *sql.Tx, userID int64, operation string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO user_operations(user_id, operation_type) VALUES ($1,$2)`, userID, operation)
	return err
}

func lockProducts(ctx context.Context, tx *sql.Tx, items []api.OrderItemInput, previous []api.OrderItemResponse) error {
	ids := make([]int64, 0, len(items)+len(previous))
	for _, item := range items {
		ids = append(ids, item.ProductId)
	}
	for _, item := range previous {
		ids = append(ids, item.ProductId)
	}
	_, err := tx.ExecContext(ctx, `SELECT id FROM products WHERE id=ANY($1) ORDER BY id FOR UPDATE`, ids)
	return err
}

func restoreStock(ctx context.Context, tx *sql.Tx, items []api.OrderItemResponse) error {
	for _, item := range items {
		if _, err := tx.ExecContext(ctx, `UPDATE products SET stock=stock+$2 WHERE id=$1`, item.ProductId, item.Quantity); err != nil {
			return err
		}
	}
	return nil
}

func reserveItems(ctx context.Context, tx *sql.Tx, items []api.OrderItemInput) ([]api.OrderItemResponse, decimal.Decimal, error) {
	quantities := map[int64]int{}
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		quantities[item.ProductId] += item.Quantity
		ids = append(ids, item.ProductId)
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+productColumns+` FROM products WHERE id=ANY($1) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, decimal.Zero, err
	}
	defer rows.Close()
	products := map[int64]api.ProductResponse{}
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, decimal.Zero, err
		}
		products[p.Id] = p
	}
	if err := rows.Err(); err != nil {
		return nil, decimal.Zero, err
	}
	for _, item := range items {
		p, exists := products[item.ProductId]
		if !exists {
			return nil, decimal.Zero, fail(404, "PRODUCT_NOT_FOUND", "Товар не найден")
		}
		if p.Status != "ACTIVE" {
			return nil, decimal.Zero, fail(409, "PRODUCT_INACTIVE", "Товар неактивен", map[string]interface{}{"product_id": item.ProductId})
		}
	}
	shortages := []map[string]interface{}{}
	for id, qty := range quantities {
		if qty > products[id].Stock {
			shortages = append(shortages, map[string]interface{}{"product_id": id, "requested": qty, "available": products[id].Stock})
		}
	}
	if len(shortages) > 0 {
		return nil, decimal.Zero, fail(409, "INSUFFICIENT_STOCK", "Недостаточно товара на складе", map[string]interface{}{"products": shortages})
	}
	for id, qty := range quantities {
		if _, err := tx.ExecContext(ctx, `UPDATE products SET stock=stock-$2 WHERE id=$1`, id, qty); err != nil {
			return nil, decimal.Zero, err
		}
	}
	result := make([]api.OrderItemResponse, 0, len(items))
	total := decimal.Zero
	for _, item := range items {
		price := products[item.ProductId].Price
		result = append(result, api.OrderItemResponse{ProductId: item.ProductId, Quantity: item.Quantity, PriceAtOrder: price})
		total = total.Add(price.Mul(decimal.NewFromInt(int64(item.Quantity))))
	}
	return result, total, nil
}

func insertItems(ctx context.Context, tx *sql.Tx, orderID int64, items []api.OrderItemResponse) error {
	for _, item := range items {
		if _, err := tx.ExecContext(ctx, `INSERT INTO order_items(order_id, product_id, quantity, price_at_order) VALUES ($1,$2,$3,$4)`,
			orderID, item.ProductId, item.Quantity, item.PriceAtOrder.String()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) CreateOrder(w http.ResponseWriter, r *http.Request) {
	if err := requireRole(r, "USER", "ADMIN"); err != nil {
		reply(w, 0, nil, err)
		return
	}
	body, err := decode[api.OrderCreate](r)
	if err != nil {
		reply(w, 0, nil, err)
		return
	}
	value, err := s.transaction(r.Context(), func(tx *sql.Tx) (any, error) {
		ctx, user := r.Context(), current(r).ID
		if err := lockUser(ctx, tx, user); err != nil {
			return nil, err
		}
		if err := s.checkInterval(ctx, tx, user, "CREATE_ORDER"); err != nil {
			return nil, err
		}
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM orders WHERE user_id=$1 AND status IN ('CREATED','PAYMENT_PENDING'))`, user).Scan(&active); err != nil {
			return nil, err
		}
		if active {
			return nil, fail(409, "ORDER_HAS_ACTIVE", "У пользователя уже есть активный заказ")
		}
		items, subtotal, err := reserveItems(ctx, tx, body.Items)
		if err != nil {
			return nil, err
		}
		var promoID *int64
		discount := decimal.Zero
		if body.PromoCode != nil {
			promo, err := lockPromo(r, tx, `code=$1`, *body.PromoCode)
			if err != nil {
				return nil, err
			}
			discount, err = promoDiscount(promo, subtotal, false)
			if err != nil {
				return nil, err
			}
			promoID = &promo.Id
			if _, err := tx.ExecContext(ctx, `UPDATE promo_codes SET current_uses=current_uses+1 WHERE id=$1`, promo.Id); err != nil {
				return nil, err
			}
		}
		var id int64
		err = tx.QueryRowContext(ctx, `INSERT INTO orders(user_id, promo_code_id, total_amount, discount_amount) VALUES ($1,$2,$3,$4) RETURNING id`,
			user, promoID, subtotal.Sub(discount).String(), discount.String()).Scan(&id)
		if err != nil {
			return nil, err
		}
		if err := insertItems(ctx, tx, id, items); err != nil {
			return nil, err
		}
		if err := recordOperation(ctx, tx, user, "CREATE_ORDER"); err != nil {
			return nil, err
		}
		return orderByID(ctx, tx, id, false)
	})
	reply(w, 201, value, err)
}

func (s *Server) GetOrder(w http.ResponseWriter, r *http.Request, id int64) {
	if err := requireRole(r, "USER", "ADMIN"); err != nil {
		reply(w, 0, nil, err)
		return
	}
	order, err := orderByID(r.Context(), s.db, id, false)
	if err == nil {
		err = ownsOrder(r, order)
	}
	reply(w, 200, order, err)
}

func (s *Server) UpdateOrder(w http.ResponseWriter, r *http.Request, id int64) {
	if err := requireRole(r, "USER", "ADMIN"); err != nil {
		reply(w, 0, nil, err)
		return
	}
	body, err := decode[api.OrderUpdate](r)
	if err != nil {
		reply(w, 0, nil, err)
		return
	}
	value, err := s.transaction(r.Context(), func(tx *sql.Tx) (any, error) {
		ctx := r.Context()
		if err := lockUser(ctx, tx, current(r).ID); err != nil {
			return nil, err
		}
		order, err := orderByID(ctx, tx, id, true)
		if err != nil {
			return nil, err
		}
		if err := ownsOrder(r, order); err != nil {
			return nil, err
		}
		if order.Status != "CREATED" {
			return nil, fail(409, "INVALID_STATE_TRANSITION", "Обновление разрешено только для CREATED")
		}
		if err := s.checkInterval(ctx, tx, current(r).ID, "UPDATE_ORDER"); err != nil {
			return nil, err
		}
		if err := lockProducts(ctx, tx, body.Items, order.Items); err != nil {
			return nil, err
		}
		if err := restoreStock(ctx, tx, order.Items); err != nil {
			return nil, err
		}
		items, subtotal, err := reserveItems(ctx, tx, body.Items)
		if err != nil {
			return nil, err
		}
		discount := decimal.Zero
		if order.PromoCodeId != nil {
			promo, err := lockPromo(r, tx, `id=$1`, *order.PromoCodeId)
			if err != nil {
				return nil, err
			}
			discount, err = promoDiscount(promo, subtotal, true)
			var p *problem
			if errors.As(err, &p) && p.body.ErrorCode == "PROMO_CODE_MIN_AMOUNT" {
				order.PromoCodeId = nil
				if _, err := tx.ExecContext(ctx, `UPDATE promo_codes SET current_uses=current_uses-1 WHERE id=$1`, promo.Id); err != nil {
					return nil, err
				}
			} else if err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE orders SET promo_code_id=$2, total_amount=$3, discount_amount=$4 WHERE id=$1`, id, order.PromoCodeId, subtotal.Sub(discount).String(), discount.String()); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM order_items WHERE order_id=$1`, id); err != nil {
			return nil, err
		}
		if err := insertItems(ctx, tx, id, items); err != nil {
			return nil, err
		}
		if err := recordOperation(ctx, tx, current(r).ID, "UPDATE_ORDER"); err != nil {
			return nil, err
		}
		return orderByID(ctx, tx, id, false)
	})
	reply(w, 200, value, err)
}

func (s *Server) CancelOrder(w http.ResponseWriter, r *http.Request, id int64) {
	if err := requireRole(r, "USER", "ADMIN"); err != nil {
		reply(w, 0, nil, err)
		return
	}
	value, err := s.transaction(r.Context(), func(tx *sql.Tx) (any, error) {
		ctx := r.Context()
		order, err := orderByID(ctx, tx, id, true)
		if err != nil {
			return nil, err
		}
		if err := ownsOrder(r, order); err != nil {
			return nil, err
		}
		if order.Status != "CREATED" && order.Status != "PAYMENT_PENDING" {
			return nil, fail(409, "INVALID_STATE_TRANSITION", "Отмена разрешена только для CREATED/PAYMENT_PENDING")
		}
		if err := lockProducts(ctx, tx, nil, order.Items); err != nil {
			return nil, err
		}
		if err := restoreStock(ctx, tx, order.Items); err != nil {
			return nil, err
		}
		if order.PromoCodeId != nil {
			if _, err := lockPromo(r, tx, `id=$1`, *order.PromoCodeId); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE promo_codes SET current_uses=current_uses-1 WHERE id=$1`, *order.PromoCodeId); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE orders SET status='CANCELED' WHERE id=$1`, id); err != nil {
			return nil, err
		}
		return orderByID(ctx, tx, id, false)
	})
	reply(w, 200, value, err)
}

func (s *Server) ChangeOrderStatus(w http.ResponseWriter, r *http.Request, id int64) {
	if err := requireRole(r, "ADMIN"); err != nil {
		reply(w, 0, nil, err)
		return
	}
	body, err := decode[api.OrderStatusUpdate](r)
	if err != nil {
		reply(w, 0, nil, err)
		return
	}
	value, err := s.transaction(r.Context(), func(tx *sql.Tx) (any, error) {
		order, err := orderByID(r.Context(), tx, id, true)
		if err != nil {
			return nil, err
		}
		next := map[api.OrderStatus]api.OrderStatus{"CREATED": "PAYMENT_PENDING", "PAYMENT_PENDING": "PAID", "PAID": "SHIPPED", "SHIPPED": "COMPLETED"}
		if target, ok := next[order.Status]; !ok || target != body.Status {
			return nil, fail(409, "INVALID_STATE_TRANSITION", "Недопустимый переход состояния")
		}
		if _, err := tx.ExecContext(r.Context(), `UPDATE orders SET status=$2 WHERE id=$1`, id, body.Status); err != nil {
			return nil, err
		}
		return orderByID(r.Context(), tx, id, false)
	})
	reply(w, 200, value, err)
}
