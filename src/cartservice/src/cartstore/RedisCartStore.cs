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

using System;
using System.Linq;
using System.Threading.Tasks;
using Grpc.Core;
using Google.Protobuf;
using StackExchange.Redis;

namespace cartservice.cartstore
{
    /// <summary>
    /// Cart store backed by a real Redis instance.
    /// </summary>
    /// <remarks>
    /// Adding an item is a read-modify-write. Issued as a plain GET followed by a
    /// SET it is not atomic: two concurrent AddItem calls for the same user both
    /// read the same pre-state and the second write silently discards the first.
    /// Every write below is therefore guarded by a WATCH-equivalent condition on
    /// the value that was read, and retried when another writer got there first
    /// (RC-013, RC-055).
    /// </remarks>
    public class RedisCartStore : ICartStore
    {
        private const int MaxWriteAttempts = 5;

        private readonly IConnectionMultiplexer _redis;

        public RedisCartStore(IConnectionMultiplexer redis)
        {
            _redis = redis;
        }

        public async Task AddItemAsync(string userId, string productId, int quantity)
        {
            Console.WriteLine($"AddItemAsync called with userId={userId}, productId={productId}, quantity={quantity}");

            try
            {
                var db = _redis.GetDatabase();
                RedisKey key = userId;

                for (int attempt = 0; attempt < MaxWriteAttempts; attempt++)
                {
                    RedisValue observed = await db.StringGetAsync(key);

                    Hipstershop.Cart cart;
                    if (observed.IsNullOrEmpty)
                    {
                        cart = new Hipstershop.Cart { UserId = userId };
                    }
                    else
                    {
                        cart = Hipstershop.Cart.Parser.ParseFrom((byte[])observed);
                    }

                    var existingItem = cart.Items.SingleOrDefault(i => i.ProductId == productId);
                    if (existingItem == null)
                    {
                        cart.Items.Add(new Hipstershop.CartItem { ProductId = productId, Quantity = quantity });
                    }
                    else
                    {
                        existingItem.Quantity += quantity;
                    }

                    // Commit only if nobody else wrote this key since we read it.
                    var transaction = db.CreateTransaction();
                    transaction.AddCondition(observed.IsNullOrEmpty
                        ? Condition.KeyNotExists(key)
                        : Condition.StringEqual(key, observed));
                    _ = transaction.StringSetAsync(key, cart.ToByteArray());

                    if (await transaction.ExecuteAsync())
                    {
                        return;
                    }
                }

                throw new RpcException(new Status(StatusCode.Aborted,
                    $"Could not update cart for {userId} after {MaxWriteAttempts} attempts due to concurrent writes."));
            }
            catch (RpcException)
            {
                throw;
            }
            catch (Exception ex)
            {
                throw new RpcException(new Status(StatusCode.FailedPrecondition, $"Can't access cart storage. {ex}"));
            }
        }

        public async Task EmptyCartAsync(string userId)
        {
            Console.WriteLine($"EmptyCartAsync called with userId={userId}");

            try
            {
                var db = _redis.GetDatabase();
                var cart = new Hipstershop.Cart();
                await db.StringSetAsync(userId, cart.ToByteArray());
            }
            catch (Exception ex)
            {
                throw new RpcException(new Status(StatusCode.FailedPrecondition, $"Can't access cart storage. {ex}"));
            }
        }

        public async Task<Hipstershop.Cart> GetCartAsync(string userId)
        {
            Console.WriteLine($"GetCartAsync called with userId={userId}");

            try
            {
                var db = _redis.GetDatabase();
                RedisValue value = await db.StringGetAsync(userId);

                if (!value.IsNullOrEmpty)
                {
                    return Hipstershop.Cart.Parser.ParseFrom((byte[])value);
                }

                // We decided to return empty cart in cases when user wasn't in the cache before
                return new Hipstershop.Cart();
            }
            catch (Exception ex)
            {
                throw new RpcException(new Status(StatusCode.FailedPrecondition, $"Can't access cart storage. {ex}"));
            }
        }

        public bool Ping()
        {
            try
            {
                return true;
            }
            catch (Exception)
            {
                return false;
            }
        }
    }
}
