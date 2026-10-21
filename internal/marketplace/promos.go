package marketplace

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/shopspring/decimal"
	"marketplace/catalog-service/internal/api"
)

const promoColumns = `id, code, discount_type, discount_value::text, min_order_amount::text, max_uses, current_uses, valid_from, valid_until, active`

func scanPromo(row rowScanner) (api.PromoCodeResponse, error) {
	var p api.PromoCodeResponse
	err := row.Scan(&p.Id, &p.Code, &p.DiscountType, &p.DiscountValue, &p.MinOrderAmount, &p.MaxUses, &p.CurrentUses, &p.ValidFrom, &p.ValidUntil, &p.Active)
	return p, err
}

func (s *Server) CreatePromoCode(w http.ResponseWriter, r *http.Request) {
	if err := requireRole(r, "SELLER", "ADMIN"); err != nil {
		reply(w, 0, nil, err)
		return
	}
	body, err := decode[api.PromoCodeCreate](r)
	if err != nil {
		reply(w, 0, nil, err)
		return
	}
	if body.ValidUntil.Before(body.ValidFrom) {
		reply(w, 0, nil, fail(400, "VALIDATION_ERROR", "Неверный период действия", map[string]interface{}{"violations": []string{"valid_until: должно быть не раньше valid_from"}}))
		return
	}
	p, err := scanPromo(s.db.QueryRowContext(r.Context(), `INSERT INTO promo_codes(code, discount_type, discount_value, min_order_amount, max_uses, valid_from, valid_until, active)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+promoColumns,
		body.Code, body.DiscountType, body.DiscountValue.String(), body.MinOrderAmount.String(), body.MaxUses, body.ValidFrom, body.ValidUntil, body.Active))
	if uniqueViolation(err) {
		err = fail(409, "PROMO_CODE_EXISTS", "Промокод уже существует")
	}
	reply(w, 201, p, err)
}

func promoDiscount(p api.PromoCodeResponse, amount decimal.Decimal, existing bool) (decimal.Decimal, error) {
	now := time.Now()
	uses := p.CurrentUses
	if existing {
		uses--
	}
	if !p.Active || uses >= p.MaxUses || now.Before(p.ValidFrom) || now.After(p.ValidUntil) {
		return decimal.Zero, fail(422, "PROMO_CODE_INVALID", "Промокод недействителен")
	}
	if amount.LessThan(p.MinOrderAmount) {
		return decimal.Zero, fail(422, "PROMO_CODE_MIN_AMOUNT", "Сумма ниже минимальной для промокода")
	}
	if p.DiscountType == "FIXED_AMOUNT" {
		return decimal.Min(p.DiscountValue, amount), nil
	}
	if p.DiscountValue.GreaterThan(decimal.NewFromInt(70)) {
		return decimal.Zero, nil
	}
	return amount.Mul(p.DiscountValue).Div(decimal.NewFromInt(100)).Round(2), nil
}

func lockPromo(r *http.Request, tx *sql.Tx, query string, value any) (api.PromoCodeResponse, error) {
	p, err := scanPromo(tx.QueryRowContext(r.Context(), `SELECT `+promoColumns+` FROM promo_codes WHERE `+query+` FOR UPDATE`, value))
	if errors.Is(err, sql.ErrNoRows) {
		err = fail(422, "PROMO_CODE_INVALID", "Промокод не найден")
	}
	return p, err
}
