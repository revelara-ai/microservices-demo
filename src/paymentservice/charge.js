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

const cardValidator = require('simple-card-validator');
const { v4: uuidv4 } = require('uuid');
const pino = require('pino');

const logger = pino({
  name: 'paymentservice-charge',
  messageKey: 'message',
  formatters: {
    level (logLevelString, logLevelNum) {
      return { severity: logLevelString }
    }
  }
});


// Idempotency window for duplicate-charge suppression (R-026). A repeated
// Charge carrying the same idempotency_key inside this window returns the
// original transaction instead of charging the card again. The cache is
// in-memory because this service simulates the processor; a real integration
// would use the processor's idempotency support or a shared store.
const IDEMPOTENCY_TTL_MS = 10 * 60 * 1000;
const recentTransactions = new Map();

function sweepExpiredTransactions () {
  const now = Date.now();
  for (const [key, entry] of recentTransactions) {
    if (now - entry.timestamp > IDEMPOTENCY_TTL_MS) {
      recentTransactions.delete(key);
    }
  }
}

class CreditCardError extends Error {
  constructor (message) {
    super(message);
    this.code = 400; // Invalid argument error
  }
}

class InvalidCreditCard extends CreditCardError {
  constructor (cardType) {
    super(`Credit card info is invalid`);
  }
}

class UnacceptedCreditCard extends CreditCardError {
  constructor (cardType) {
    super(`Sorry, we cannot process ${cardType} credit cards. Only VISA or MasterCard is accepted.`);
  }
}

class ExpiredCreditCard extends CreditCardError {
  constructor (number, month, year) {
    super(`Your credit card (ending ${number.substr(-4)}) expired on ${month}/${year}`);
  }
}

/**
 * Verifies the credit card number and (pretend) charges the card.
 *
 * @param {*} request
 * @return transaction_id - a random uuid.
 */
module.exports = function charge (request) {
  const { amount, credit_card: creditCard, idempotency_key: idempotencyKey } = request;

  if (idempotencyKey) {
    sweepExpiredTransactions();
    const prior = recentTransactions.get(idempotencyKey);
    if (prior) {
      logger.info(`Duplicate charge suppressed for idempotency key ${idempotencyKey}; returning original transaction ${prior.transactionId}`);
      return { transaction_id: prior.transactionId };
    }
  }

  const cardNumber = creditCard.credit_card_number;
  const cardInfo = cardValidator(cardNumber);
  const {
    card_type: cardType,
    valid
  } = cardInfo.getCardDetails();

  if (!valid) { throw new InvalidCreditCard(); }

  // Only VISA and mastercard is accepted, other card types (AMEX, dinersclub) will
  // throw UnacceptedCreditCard error.
  if (!(cardType === 'visa' || cardType === 'mastercard')) { throw new UnacceptedCreditCard(cardType); }

  // Also validate expiration is > today.
  const currentMonth = new Date().getMonth() + 1;
  const currentYear = new Date().getFullYear();
  const { credit_card_expiration_year: year, credit_card_expiration_month: month } = creditCard;
  if ((currentYear * 12 + currentMonth) > (year * 12 + month)) { throw new ExpiredCreditCard(cardNumber.replace('-', ''), month, year); }

  logger.info(`Transaction processed: ${cardType} ending ${cardNumber.substr(-4)} \
    Amount: ${amount.currency_code}${amount.units}.${amount.nanos}`);

  const transactionId = uuidv4();
  if (idempotencyKey) {
    recentTransactions.set(idempotencyKey, { transactionId, timestamp: Date.now() });
  }
  return { transaction_id: transactionId };
};

// Exposed for tests only.
module.exports._recentTransactions = recentTransactions;
module.exports._idempotencyTtlMs = IDEMPOTENCY_TTL_MS;
