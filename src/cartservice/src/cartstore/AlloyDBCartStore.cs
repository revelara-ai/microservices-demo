// Copyright 2021 Google LLC
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
using System.Text.RegularExpressions;
using Grpc.Core;
using Npgsql;
using Microsoft.Extensions.Configuration;
using System.Threading.Tasks;
using Google.Api.Gax.ResourceNames;
using Google.Cloud.SecretManager.V1;

namespace cartservice.cartstore
{
    public class AlloyDBCartStore : ICartStore, IAsyncDisposable
    {
        // Bound the pool and every statement. Without these a slow query holds
        // connections until the pool saturates, and a saturated pool is what
        // turns one slow query into a service-wide outage (RC-055, RC-019).
        private const int MaxPoolSize = 50;
        private const int ConnectTimeoutSeconds = 5;
        private const int CommandTimeoutSeconds = 5;

        private readonly string tableName;

        // NpgsqlDataSource owns the connection pool and is designed to live for
        // the process. Creating one per RPC — as this store used to — builds and
        // tears down a whole pool on every call (RC-055).
        private readonly NpgsqlDataSource dataSource;

        public AlloyDBCartStore(IConfiguration configuration)
        {
            // Create a Cloud Secrets client.
            SecretManagerServiceClient client = SecretManagerServiceClient.Create();
            var projectId = configuration["PROJECT_ID"];
            var secretId = configuration["ALLOYDB_SECRET_NAME"];
            SecretVersionName secretVersionName = new SecretVersionName(projectId, secretId, "latest");

            AccessSecretVersionResponse result = client.AccessSecretVersion(secretVersionName);
            // Convert the payload to a string. Payloads are bytes by default.
            string alloyDBPassword = result.Payload.Data.ToStringUtf8().TrimEnd('\r', '\n');

            // TODO: Create a separate user for connecting within the application
            // rather than using our superuser
            string alloyDBUser = "postgres";
            string databaseName = configuration["ALLOYDB_DATABASE_NAME"];
            // TODO: Consider splitting workloads into read vs. write and take
            // advantage of the AlloyDB read pools
            string primaryIPAddress = configuration["ALLOYDB_PRIMARY_IP"];

            var connectionStringBuilder = new NpgsqlConnectionStringBuilder
            {
                Host = primaryIPAddress,
                Username = alloyDBUser,
                Password = alloyDBPassword,
                Database = databaseName,
                MaxPoolSize = MaxPoolSize,
                Timeout = ConnectTimeoutSeconds,
                CommandTimeout = CommandTimeoutSeconds
            };

            tableName = QuoteIdentifier(configuration["ALLOYDB_TABLE_NAME"]);
            dataSource = NpgsqlDataSource.Create(connectionStringBuilder.ConnectionString);
        }

        // A table name is an identifier, so it cannot be a query parameter. It
        // comes from configuration rather than from a request, but quote it and
        // reject anything that is not a plain identifier so it can never be a
        // vector for statement injection (RC-044).
        private static string QuoteIdentifier(string identifier)
        {
            if (string.IsNullOrWhiteSpace(identifier) || !Regex.IsMatch(identifier, "^[A-Za-z_][A-Za-z0-9_]*$"))
            {
                throw new ArgumentException($"ALLOYDB_TABLE_NAME is not a valid identifier: {identifier}");
            }
            return "\"" + identifier + "\"";
        }

        public async Task AddItemAsync(string userId, string productId, int quantity)
        {
            Console.WriteLine($"AddItemAsync for {userId} called");
            try
            {
                // One atomic statement. The previous SELECT-then-INSERT computed
                // the new quantity in process memory, so two concurrent AddItem
                // calls for the same user both read the same pre-state and the
                // second write silently discarded the first (RC-013, RC-055).
                var upsertCmd = $@"
                    INSERT INTO {tableName} (userId, productId, quantity)
                    VALUES (@userId, @productId, @quantity)
                    ON CONFLICT (userId, productId)
                    DO UPDATE SET quantity = {tableName}.quantity + EXCLUDED.quantity;
                ";

                await using var cmd = dataSource.CreateCommand(upsertCmd);
                cmd.Parameters.AddWithValue("userId", userId);
                cmd.Parameters.AddWithValue("productId", productId);
                cmd.Parameters.AddWithValue("quantity", quantity);
                await cmd.ExecuteNonQueryAsync();
            }
            catch (Exception ex)
            {
                throw new RpcException(
                    new Status(StatusCode.FailedPrecondition, $"Unable to access cart storage due to an internal error. {ex}"));
            }
        }

        public async Task<Hipstershop.Cart> GetCartAsync(string userId)
        {
            Console.WriteLine($"GetCartAsync called for userId={userId}");
            Hipstershop.Cart cart = new();
            cart.UserId = userId;
            try
            {
                await using var cmd = dataSource.CreateCommand(
                    $"SELECT productId, quantity FROM {tableName} WHERE userId = @userId");
                cmd.Parameters.AddWithValue("userId", userId);

                await using var reader = await cmd.ExecuteReaderAsync();
                while (await reader.ReadAsync())
                {
                    Hipstershop.CartItem item = new()
                    {
                        ProductId = reader.GetString(0),
                        Quantity = reader.GetInt32(1)
                    };
                    cart.Items.Add(item);
                }
            }
            catch (Exception ex)
            {
                throw new RpcException(
                    new Status(StatusCode.FailedPrecondition, $"Unable to access cart storage due to an internal error. {ex}"));
            }
            return cart;
        }

        public async Task EmptyCartAsync(string userId)
        {
            Console.WriteLine($"EmptyCartAsync called for userId={userId}");

            try
            {
                await using var cmd = dataSource.CreateCommand(
                    $"DELETE FROM {tableName} WHERE userId = @userId");
                cmd.Parameters.AddWithValue("userId", userId);
                await cmd.ExecuteNonQueryAsync();
            }
            catch (Exception ex)
            {
                throw new RpcException(
                    new Status(StatusCode.FailedPrecondition, $"Unable to access cart storage due to an internal error. {ex}"));
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

        public async ValueTask DisposeAsync()
        {
            await dataSource.DisposeAsync();
            GC.SuppressFinalize(this);
        }
    }
}
