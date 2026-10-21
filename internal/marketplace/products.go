package marketplace

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"marketplace/catalog-service/internal/api"
)

const productColumns = `id, name, description, price::text, stock, category, status, seller_id, created_at, updated_at`

type rowScanner interface{ Scan(...any) error }

func scanProduct(row rowScanner) (api.ProductResponse, error) {
	var p api.ProductResponse
	err := row.Scan(&p.Id, &p.Name, &p.Description, &p.Price, &p.Stock, &p.Category, &p.Status, &p.SellerId, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = fail(404, "PRODUCT_NOT_FOUND", "Товар не найден")
	}
	return p, err
}

func (s *Server) productByID(ctx context.Context, id int64) (api.ProductResponse, error) {
	return scanProduct(s.db.QueryRowContext(ctx, `SELECT `+productColumns+` FROM products WHERE id=$1`, id))
}

func (s *Server) CreateProduct(w http.ResponseWriter, r *http.Request) {
	if err := requireRole(r, "SELLER", "ADMIN"); err != nil {
		reply(w, 0, nil, err)
		return
	}
	body, err := decode[api.ProductCreate](r)
	if err != nil {
		reply(w, 0, nil, err)
		return
	}
	p, err := scanProduct(s.db.QueryRowContext(r.Context(), `INSERT INTO products(name, description, price, stock, category, status, seller_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+productColumns,
		body.Name, body.Description, body.Price.String(), body.Stock, body.Category, body.Status, current(r).ID))
	reply(w, 201, p, err)
}

func (s *Server) GetProduct(w http.ResponseWriter, r *http.Request, id int64) {
	p, err := s.productByID(r.Context(), id)
	reply(w, 200, p, err)
}

func (s *Server) ListProducts(w http.ResponseWriter, r *http.Request, params api.ListProductsParams) {
	page, size := 0, 20
	if params.Page != nil {
		page = *params.Page
	}
	if params.Size != nil {
		size = *params.Size
	}
	result := api.ProductPage{Items: []api.ProductResponse{}, Page: page, Size: size}
	where := ` WHERE ($1::product_status IS NULL OR status=$1::product_status) AND ($2::text IS NULL OR category=$2)`
	err := s.db.QueryRowContext(r.Context(), `SELECT count(*) FROM products`+where, params.Status, params.Category).Scan(&result.TotalElements)
	if err != nil {
		reply(w, 0, nil, err)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `SELECT `+productColumns+` FROM products`+where+` ORDER BY id LIMIT $3 OFFSET $4`, params.Status, params.Category, size, int64(page)*int64(size))
	if err != nil {
		reply(w, 0, nil, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		p, scanErr := scanProduct(rows)
		if scanErr != nil {
			reply(w, 0, nil, scanErr)
			return
		}
		result.Items = append(result.Items, p)
	}
	reply(w, 200, result, rows.Err())
}

func (s *Server) editableProduct(r *http.Request, id int64) error {
	if err := requireRole(r, "SELLER", "ADMIN"); err != nil {
		return err
	}
	p, err := s.productByID(r.Context(), id)
	if err != nil {
		return err
	}
	if current(r).Role != "ADMIN" && p.SellerId != current(r).ID {
		return fail(403, "ACCESS_DENIED", "Товар принадлежит другому продавцу")
	}
	return nil
}

func (s *Server) UpdateProduct(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.editableProduct(r, id); err != nil {
		reply(w, 0, nil, err)
		return
	}
	body, err := decode[api.ProductUpdate](r)
	if err != nil {
		reply(w, 0, nil, err)
		return
	}
	p, err := scanProduct(s.db.QueryRowContext(r.Context(), `UPDATE products SET name=$2, description=$3, price=$4, stock=$5, category=$6, status=$7 WHERE id=$1 RETURNING `+productColumns,
		id, body.Name, body.Description, body.Price.String(), body.Stock, body.Category, body.Status))
	reply(w, 200, p, err)
}

func (s *Server) DeleteProduct(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.editableProduct(r, id); err != nil {
		reply(w, 0, nil, err)
		return
	}
	_, err := s.db.ExecContext(r.Context(), `UPDATE products SET status='ARCHIVED' WHERE id=$1`, id)
	reply(w, 204, nil, err)
}
