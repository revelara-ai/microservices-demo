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

const { v4: uuidv4 } = require('uuid');
const pino = require('pino');

const logger = pino({
  name: 'paymentservice-refund',
  messageKey: 'message',
  formatters: {
    level (logLevelString, logLevelNum) {
      return { severity: logLevelString }
    }
  }
});

class RefundError extends Error {
  constructor (message) {
    super(message);
    this.code = 400; // Invalid argument error
  }
}

// Reversals are the compensating transaction for a checkout saga that failed
// after the card was charged. Callers retry them, so the same transaction_id
// MUST NOT produce two refunds. Keep the issued refund ids so a retry replays
// the first answer instead of moving money twice.
//
// The map is bounded and per-replica: it is a defence against the retry storm
// of a single failing checkout, not a durable ledger. A real payment processor
// would enforce idempotency on transaction_id server-side; the reconciliation
// record written by checkoutservice covers what this cannot.
const MAX_TRACKED_REFUNDS = 10000;
const issuedRefunds = new Map();

function rememberRefund (transactionId, refundId) {
  if (issuedRefunds.size >= MAX_TRACKED_REFUNDS) {
    // Map preserves insertion order, so the first key is the oldest.
    issuedRefunds.delete(issuedRefunds.keys().next().value);
  }
  issuedRefunds.set(transactionId, refundId);
}

/**
 * Reverses a charge previously made by charge().
 *
 * Idempotent on transaction_id: repeating a refund returns the refund_id
 * issued the first time rather than issuing a second reversal.
 *
 * @param {*} request  { transaction_id, amount, reason }
 * @return refund_id - a random uuid, stable per transaction_id.
 */
module.exports = function refund (request) {
  const { transaction_id: transactionId, amount, reason } = request;

  if (!transactionId) {
    throw new RefundError('transaction_id is required to reverse a charge');
  }

  const existing = issuedRefunds.get(transactionId);
  if (existing) {
    logger.info({
      event: 'refund_replayed',
      transaction_id: transactionId,
      refund_id: existing
    }, `Refund for transaction ${transactionId} already issued; replaying ${existing}`);
    return { refund_id: existing };
  }

  const refundId = uuidv4();
  rememberRefund(transactionId, refundId);

  logger.info({
    event: 'refund_processed',
    transaction_id: transactionId,
    refund_id: refundId,
    amount_units: amount ? amount.units : undefined,
    amount_nanos: amount ? amount.nanos : undefined,
    currency_code: amount ? amount.currency_code : undefined,
    reason: reason
  }, `Reversed transaction ${transactionId} (refund ${refundId})`);

  return { refund_id: refundId };
};

module.exports.RefundError = RefundError;
