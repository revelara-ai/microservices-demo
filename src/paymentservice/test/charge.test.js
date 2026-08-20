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

const { test, beforeEach } = require('node:test');
const assert = require('node:assert');

const charge = require('../charge');

const VALID_VISA = '4111111111111111';

function validRequest (overrides = {}) {
  return {
    amount: { currency_code: 'USD', units: 30, nanos: 0 },
    credit_card: {
      credit_card_number: VALID_VISA,
      credit_card_expiration_month: 1,
      credit_card_expiration_year: new Date().getFullYear() + 2,
      credit_card_cvv: 123
    },
    ...overrides
  };
}

beforeEach(() => {
  charge._recentTransactions.clear();
});

test('valid charge returns a transaction id', () => {
  const response = charge(validRequest());
  assert.ok(response.transaction_id, 'expected a transaction_id');
});

test('repeated charge with same idempotency key returns original transaction', () => {
  const first = charge(validRequest({ idempotency_key: 'order-123' }));
  const second = charge(validRequest({ idempotency_key: 'order-123' }));
  assert.strictEqual(second.transaction_id, first.transaction_id);
  assert.strictEqual(charge._recentTransactions.size, 1);
});

test('distinct idempotency keys charge independently', () => {
  const first = charge(validRequest({ idempotency_key: 'order-a' }));
  const second = charge(validRequest({ idempotency_key: 'order-b' }));
  assert.notStrictEqual(second.transaction_id, first.transaction_id);
});

test('charges without an idempotency key are never deduplicated', () => {
  const first = charge(validRequest());
  const second = charge(validRequest());
  assert.notStrictEqual(second.transaction_id, first.transaction_id);
  assert.strictEqual(charge._recentTransactions.size, 0);
});

test('expired idempotency entries are swept and re-charge', () => {
  const first = charge(validRequest({ idempotency_key: 'order-old' }));
  const entry = charge._recentTransactions.get('order-old');
  entry.timestamp = Date.now() - charge._idempotencyTtlMs - 1;
  const second = charge(validRequest({ idempotency_key: 'order-old' }));
  assert.notStrictEqual(second.transaction_id, first.transaction_id);
});

test('invalid card still throws with an idempotency key and caches nothing', () => {
  const bad = validRequest({ idempotency_key: 'order-bad' });
  bad.credit_card.credit_card_number = '1234';
  assert.throws(() => charge(bad));
  assert.strictEqual(charge._recentTransactions.size, 0);
});
