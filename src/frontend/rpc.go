// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"time"

	pb "github.com/GoogleCloudPlatform/microservices-demo/src/frontend/genproto"

	"github.com/pkg/errors"
)

const (
	avoidNoopCurrencyConversionRPC = false

	// Per-backend deadlines bound how long a single gRPC call may block a
	// frontend HTTP handler. Without these the request context (which has no
	// deadline of its own) lets a slow backend pin frontend goroutines until
	// the client disconnects (R-001).
	currencyRPCTimeout       = 1 * time.Second
	productCatalogRPCTimeout = 1 * time.Second
	cartRPCTimeout           = 2 * time.Second
	shippingRPCTimeout       = 2 * time.Second
	recommendationRPCTimeout = 3 * time.Second
)

func (fe *frontendServer) getCurrencies(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, currencyRPCTimeout)
	defer cancel()

	result, err := cbExecute(ctx, fe.currencyCB, func() (any, error) {
		return pb.NewCurrencyServiceClient(fe.currencySvcConn).
			GetSupportedCurrencies(ctx, &pb.Empty{})
	})
	if err != nil {
		return nil, err
	}
	currs := result.(*pb.GetSupportedCurrenciesResponse)
	var out []string
	for _, c := range currs.CurrencyCodes {
		if _, ok := whitelistedCurrencies[c]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func (fe *frontendServer) getProducts(ctx context.Context) ([]*pb.Product, error) {
	ctx, cancel := context.WithTimeout(ctx, productCatalogRPCTimeout)
	defer cancel()

	result, err := cbExecute(ctx, fe.productCatalogCB, func() (any, error) {
		return pb.NewProductCatalogServiceClient(fe.productCatalogSvcConn).
			ListProducts(ctx, &pb.Empty{})
	})
	if err != nil {
		return nil, err
	}
	return result.(*pb.ListProductsResponse).GetProducts(), nil
}

func (fe *frontendServer) getProduct(ctx context.Context, id string) (*pb.Product, error) {
	ctx, cancel := context.WithTimeout(ctx, productCatalogRPCTimeout)
	defer cancel()

	result, err := cbExecute(ctx, fe.productCatalogCB, func() (any, error) {
		return pb.NewProductCatalogServiceClient(fe.productCatalogSvcConn).
			GetProduct(ctx, &pb.GetProductRequest{Id: id})
	})
	if err != nil {
		return nil, err
	}
	return result.(*pb.Product), nil
}

func (fe *frontendServer) getCart(ctx context.Context, userID string) ([]*pb.CartItem, error) {
	ctx, cancel := context.WithTimeout(ctx, cartRPCTimeout)
	defer cancel()

	result, err := cbExecute(ctx, fe.cartCB, func() (any, error) {
		return pb.NewCartServiceClient(fe.cartSvcConn).
			GetCart(ctx, &pb.GetCartRequest{UserId: userID})
	})
	if err != nil {
		return nil, err
	}
	return result.(*pb.Cart).GetItems(), nil
}

func (fe *frontendServer) emptyCart(ctx context.Context, userID string) error {
	ctx, cancel := context.WithTimeout(ctx, cartRPCTimeout)
	defer cancel()

	_, err := cbExecute(ctx, fe.cartCB, func() (any, error) {
		return pb.NewCartServiceClient(fe.cartSvcConn).
			EmptyCart(ctx, &pb.EmptyCartRequest{UserId: userID})
	})
	return err
}

func (fe *frontendServer) insertCart(ctx context.Context, userID, productID string, quantity int32) error {
	ctx, cancel := context.WithTimeout(ctx, cartRPCTimeout)
	defer cancel()

	_, err := cbExecute(ctx, fe.cartCB, func() (any, error) {
		return pb.NewCartServiceClient(fe.cartSvcConn).AddItem(ctx, &pb.AddItemRequest{
			UserId: userID,
			Item: &pb.CartItem{
				ProductId: productID,
				Quantity:  quantity},
		})
	})
	return err
}

func (fe *frontendServer) convertCurrency(ctx context.Context, money *pb.Money, currency string) (*pb.Money, error) {
	if avoidNoopCurrencyConversionRPC && money.GetCurrencyCode() == currency {
		return money, nil
	}
	ctx, cancel := context.WithTimeout(ctx, currencyRPCTimeout)
	defer cancel()

	result, err := cbExecute(ctx, fe.currencyCB, func() (any, error) {
		return pb.NewCurrencyServiceClient(fe.currencySvcConn).
			Convert(ctx, &pb.CurrencyConversionRequest{
				From:   money,
				ToCode: currency})
	})
	if err != nil {
		return nil, err
	}
	return result.(*pb.Money), nil
}

func (fe *frontendServer) getShippingQuote(ctx context.Context, items []*pb.CartItem, currency string) (*pb.Money, error) {
	quoteCtx, cancel := context.WithTimeout(ctx, shippingRPCTimeout)
	defer cancel()

	result, err := cbExecute(quoteCtx, fe.shippingCB, func() (any, error) {
		return pb.NewShippingServiceClient(fe.shippingSvcConn).
			GetQuote(quoteCtx, &pb.GetQuoteRequest{
				Address: nil,
				Items:   items})
	})
	if err != nil {
		return nil, err
	}
	quote := result.(*pb.GetQuoteResponse)
	// convertCurrency applies its own timeout via currencyRPCTimeout on the
	// caller's ctx; the shipping deadline above does not bound it.
	localized, err := fe.convertCurrency(ctx, quote.GetCostUsd(), currency)
	return localized, errors.Wrap(err, "failed to convert currency for shipping cost")
}

func (fe *frontendServer) getRecommendations(ctx context.Context, userID string, productIDs []string) ([]*pb.Product, error) {
	listCtx, cancel := context.WithTimeout(ctx, recommendationRPCTimeout)
	defer cancel()

	result, err := cbExecute(listCtx, fe.recommendationCB, func() (any, error) {
		return pb.NewRecommendationServiceClient(fe.recommendationSvcConn).ListRecommendations(listCtx,
			&pb.ListRecommendationsRequest{UserId: userID, ProductIds: productIDs})
	})
	if err != nil {
		return nil, err
	}
	resp := result.(*pb.ListRecommendationsResponse)
	out := make([]*pb.Product, len(resp.GetProductIds()))
	for i, v := range resp.GetProductIds() {
		// Each getProduct call has its own productCatalogRPCTimeout deadline
		// and is gated by productCatalogCB.
		p, err := fe.getProduct(ctx, v)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get recommended product info (#%s)", v)
		}
		out[i] = p
	}
	if len(out) > 4 {
		out = out[:4] // take only first four to fit the UI
	}
	return out, err
}

func (fe *frontendServer) getAd(ctx context.Context, ctxKeys []string) ([]*pb.Ad, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Millisecond*100)
	defer cancel()

	result, err := cbExecute(ctx, fe.adCB, func() (any, error) {
		return pb.NewAdServiceClient(fe.adSvcConn).GetAds(ctx, &pb.AdRequest{
			ContextKeys: ctxKeys,
		})
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to get ads")
	}
	return result.(*pb.AdResponse).GetAds(), nil
}
