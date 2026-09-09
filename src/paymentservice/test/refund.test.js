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

const test = require('node:test');
const assert = require('node:assert');

const refund = require('../refund');

const amount = { currency_code: 'USD', units: 15, nanos: 0 };

function uniqueTransactionId (label) {
  return `tx-${label}-${process.hrtime.bigint()}`;
}

// checkoutservice retries the reversal, so the same transaction must never
// produce two refunds (R-029, R-047).
test('refund is idempotent on transaction_id', () => {
  const transactionId = uniqueTransactionId('idempotent');

  const first = refund({ transaction_id: transactionId, amount, reason: 'shipping failed' });
  const second = refund({ transaction_id: transactionId, amount, reason: 'shipping failed' });
  const third = refund({ transaction_id: transactionId });

  assert.ok(first.refund_id, 'the first refund returned no refund_id');
  assert.strictEqual(second.refund_id, first.refund_id);
  assert.strictEqual(third.refund_id, first.refund_id);
});

test('distinct charges get distinct refunds', () => {
  const a = refund({ transaction_id: uniqueTransactionId('a'), amount, reason: 'shipping failed' });
  const b = refund({ transaction_id: uniqueTransactionId('b'), amount, reason: 'shipping failed' });

  assert.notStrictEqual(a.refund_id, b.refund_id);
});

test('refund rejects a request with no transaction_id', () => {
  assert.throws(() => refund({ amount, reason: 'shipping failed' }), /transaction_id is required/);
  assert.throws(() => refund({ transaction_id: '', amount }), /transaction_id is required/);
});

// A charge whose amount is unknown must still be reversible: the reversal is
// what the customer is owed, and the record can be reconciled afterwards.
test('refund succeeds without an amount', () => {
  const result = refund({ transaction_id: uniqueTransactionId('no-amount'), reason: 'shipping failed' });
  assert.ok(result.refund_id);
});
