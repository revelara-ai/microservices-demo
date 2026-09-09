// Copyright 2026 Google LLC
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
	"net"
	"sync"
	"testing"
	"time"

	pb "github.com/GoogleCloudPlatform/microservices-demo/src/checkoutservice/genproto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeBackend stands in for every downstream of the checkout saga. Only the
// behaviour PlaceOrder depends on is modelled: a one-item cart, a fixed price,
// an identity currency conversion, and a shipping call whose outcome the test
// chooses. It records the Refund calls it receives so a test can assert that
// the compensating transaction actually fired (R-029, R-047).
type fakeBackend struct {
	pb.UnimplementedCartServiceServer
	pb.UnimplementedProductCatalogServiceServer
	pb.UnimplementedCurrencyServiceServer
	pb.UnimplementedShippingServiceServer
	pb.UnimplementedPaymentServiceServer
	pb.UnimplementedEmailServiceServer

	// shipOrderErr, when set, is returned by ShipOrder to drive the failure path.
	shipOrderErr error

	// refundErr, when set, is returned by Refund to drive the reversal-failed path.
	refundErr error

	mu      sync.Mutex
	charges []*pb.ChargeRequest
	refunds []*pb.RefundRequest
}

const (
	testTransactionID = "tx-under-test"
	testProductID     = "OLJCESPC7Z"
)

func (f *fakeBackend) GetCart(_ context.Context, req *pb.GetCartRequest) (*pb.Cart, error) {
	return &pb.Cart{
		UserId: req.GetUserId(),
		Items:  []*pb.CartItem{{ProductId: testProductID, Quantity: 1}},
	}, nil
}

func (f *fakeBackend) EmptyCart(context.Context, *pb.EmptyCartRequest) (*pb.Empty, error) {
	return &pb.Empty{}, nil
}

func (f *fakeBackend) GetProduct(_ context.Context, req *pb.GetProductRequest) (*pb.Product, error) {
	return &pb.Product{
		Id:       req.GetId(),
		Name:     "Test Product",
		PriceUsd: &pb.Money{CurrencyCode: "USD", Units: 10, Nanos: 0},
	}, nil
}

// Convert is an identity conversion: the saga only needs a Money back.
func (f *fakeBackend) Convert(_ context.Context, req *pb.CurrencyConversionRequest) (*pb.Money, error) {
	from := req.GetFrom()
	return &pb.Money{
		CurrencyCode: req.GetToCode(),
		Units:        from.GetUnits(),
		Nanos:        from.GetNanos(),
	}, nil
}

func (f *fakeBackend) GetQuote(context.Context, *pb.GetQuoteRequest) (*pb.GetQuoteResponse, error) {
	return &pb.GetQuoteResponse{CostUsd: &pb.Money{CurrencyCode: "USD", Units: 5, Nanos: 0}}, nil
}

func (f *fakeBackend) ShipOrder(context.Context, *pb.ShipOrderRequest) (*pb.ShipOrderResponse, error) {
	if f.shipOrderErr != nil {
		return nil, f.shipOrderErr
	}
	return &pb.ShipOrderResponse{TrackingId: "tracking-id"}, nil
}

func (f *fakeBackend) Charge(_ context.Context, req *pb.ChargeRequest) (*pb.ChargeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.charges = append(f.charges, req)
	return &pb.ChargeResponse{TransactionId: testTransactionID}, nil
}

func (f *fakeBackend) Refund(_ context.Context, req *pb.RefundRequest) (*pb.RefundResponse, error) {
	f.mu.Lock()
	f.refunds = append(f.refunds, req)
	f.mu.Unlock()
	if f.refundErr != nil {
		return nil, f.refundErr
	}
	return &pb.RefundResponse{RefundId: "refund-id"}, nil
}

func (f *fakeBackend) SendOrderConfirmation(context.Context, *pb.SendOrderConfirmationRequest) (*pb.Empty, error) {
	return &pb.Empty{}, nil
}

func (f *fakeBackend) recordedRefunds() []*pb.RefundRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*pb.RefundRequest, len(f.refunds))
	copy(out, f.refunds)
	return out
}

// newTestCheckoutService serves fake on an in-process listener and points every
// downstream connection of a checkoutService at it.
func newTestCheckoutService(t *testing.T, fake *fakeBackend) *checkoutService {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	pb.RegisterCartServiceServer(srv, fake)
	pb.RegisterProductCatalogServiceServer(srv, fake)
	pb.RegisterCurrencyServiceServer(srv, fake)
	pb.RegisterShippingServiceServer(srv, fake)
	pb.RegisterPaymentServiceServer(srv, fake)
	pb.RegisterEmailServiceServer(srv, fake)

	go func() {
		if err := srv.Serve(lis); err != nil {
			t.Logf("test server stopped: %v", err)
		}
	}()
	t.Cleanup(func() {
		srv.Stop()
		lis.Close()
	})

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed to dial test server: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return &checkoutService{
		productCatalogSvcConn: conn,
		cartSvcConn:           conn,
		currencySvcConn:       conn,
		shippingSvcConn:       conn,
		emailSvcConn:          conn,
		paymentSvcConn:        conn,
	}
}

func testPlaceOrderRequest() *pb.PlaceOrderRequest {
	return &pb.PlaceOrderRequest{
		UserId:       "user-under-test",
		UserCurrency: "USD",
		Address:      &pb.Address{StreetAddress: "1 Test St", City: "Testville", Country: "United States"},
		Email:        "test@example.com",
		CreditCard: &pb.CreditCardInfo{
			CreditCardNumber:          "4432-8015-6152-0454",
			CreditCardCvv:             672,
			CreditCardExpirationYear:  2039,
			CreditCardExpirationMonth: 1,
		},
	}
}

// A failed shipment after a successful charge MUST reverse that charge.
// Without the compensating transaction the customer is billed for an order
// that never ships (R-029, R-047).
func TestPlaceOrderRefundsChargeWhenShipmentFails(t *testing.T) {
	fake := &fakeBackend{shipOrderErr: status.Error(codes.Unavailable, "shippingservice is down")}
	cs := newTestCheckoutService(t, fake)

	_, err := cs.PlaceOrder(context.Background(), testPlaceOrderRequest())
	if err == nil {
		t.Fatal("PlaceOrder returned no error even though ShipOrder failed")
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Errorf("PlaceOrder returned code %v, want %v", got, codes.Unavailable)
	}

	refunds := fake.recordedRefunds()
	if len(refunds) != 1 {
		t.Fatalf("got %d refund calls, want exactly 1", len(refunds))
	}
	if got := refunds[0].GetTransactionId(); got != testTransactionID {
		t.Errorf("refund carried transaction_id %q, want %q", got, testTransactionID)
	}
	// 10 USD for the item plus 5 USD shipping.
	if got := refunds[0].GetAmount().GetUnits(); got != 15 {
		t.Errorf("refund amount was %d units, want the full charge of 15", got)
	}
	if refunds[0].GetReason() == "" {
		t.Error("refund carried no reason; the reconciliation record needs one")
	}
}

// A successful checkout must never reverse the charge it just made.
func TestPlaceOrderDoesNotRefundOnSuccess(t *testing.T) {
	fake := &fakeBackend{}
	cs := newTestCheckoutService(t, fake)

	resp, err := cs.PlaceOrder(context.Background(), testPlaceOrderRequest())
	if err != nil {
		t.Fatalf("PlaceOrder failed on the happy path: %v", err)
	}
	if resp.GetOrder().GetShippingTrackingId() != "tracking-id" {
		t.Errorf("got tracking id %q, want %q", resp.GetOrder().GetShippingTrackingId(), "tracking-id")
	}
	if n := len(fake.recordedRefunds()); n != 0 {
		t.Errorf("got %d refund calls on a successful order, want 0", n)
	}
}

// When the reversal itself fails, PlaceOrder must still fail cleanly rather
// than hang or panic: the orphaned-charge record is the remaining safety net.
func TestPlaceOrderSurvivesFailedRefund(t *testing.T) {
	fake := &fakeBackend{
		shipOrderErr: status.Error(codes.Unavailable, "shippingservice is down"),
		refundErr:    status.Error(codes.Internal, "paymentservice is down"),
	}
	cs := newTestCheckoutService(t, fake)

	_, err := cs.PlaceOrder(context.Background(), testPlaceOrderRequest())
	if err == nil {
		t.Fatal("PlaceOrder returned no error even though ShipOrder failed")
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Errorf("PlaceOrder returned code %v, want %v", got, codes.Unavailable)
	}
	if n := len(fake.recordedRefunds()); n != refundAttempts {
		t.Errorf("got %d refund attempts, want %d", n, refundAttempts)
	}
}

// The reversal must not inherit cancellation from the request that triggered
// it. A client that hangs up mid-checkout must still get its money back.
func TestPlaceOrderRefundsAfterRequestContextCancelled(t *testing.T) {
	fake := &fakeBackend{shipOrderErr: status.Error(codes.Unavailable, "shippingservice is down")}
	cs := newTestCheckoutService(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	txID, err := cs.chargeCard(ctx, &pb.Money{CurrencyCode: "USD", Units: 15}, testPlaceOrderRequest().GetCreditCard())
	if err != nil {
		t.Fatalf("chargeCard failed: %v", err)
	}

	// The customer disconnects: the request context dies before the saga can
	// unwind itself.
	cancel()

	refundID, err := cs.refundCharge(ctx, txID, &pb.Money{CurrencyCode: "USD", Units: 15}, "shipping failed")
	if err != nil {
		t.Fatalf("refundCharge failed on a cancelled request context: %v", err)
	}
	if refundID != "refund-id" {
		t.Errorf("got refund id %q, want %q", refundID, "refund-id")
	}
	if n := len(fake.recordedRefunds()); n != 1 {
		t.Errorf("got %d refund calls, want 1", n)
	}
}

func TestMain(m *testing.M) {
	// Keep the retry backoff from dominating the suite runtime.
	if refundRetryDelay > time.Second {
		panic("refundRetryDelay is too long for the test suite")
	}
	m.Run()
}
