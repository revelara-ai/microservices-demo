#!/usr/bin/python
#
# Copyright 2024 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import os

from google.cloud import secretmanager_v1
from urllib.parse import unquote
from langchain_core.messages import HumanMessage, SystemMessage
from langchain_google_genai import ChatGoogleGenerativeAI, GoogleGenerativeAIEmbeddings
from flask import Flask, jsonify, request

from langchain_google_alloydb_pg import AlloyDBEngine, AlloyDBVectorStore

from prompts import (
    DESIGN_SYSTEM_PROMPT,
    VISION_SYSTEM_PROMPT,
    build_design_human_message,
    build_vector_search_query,
)
from sanitize import SanitizationError, sanitize_image_url, sanitize_message

PROJECT_ID = os.environ["PROJECT_ID"]
REGION = os.environ["REGION"]
ALLOYDB_DATABASE_NAME = os.environ["ALLOYDB_DATABASE_NAME"]
ALLOYDB_TABLE_NAME = os.environ["ALLOYDB_TABLE_NAME"]
ALLOYDB_CLUSTER_NAME = os.environ["ALLOYDB_CLUSTER_NAME"]
ALLOYDB_INSTANCE_NAME = os.environ["ALLOYDB_INSTANCE_NAME"]
ALLOYDB_SECRET_NAME = os.environ["ALLOYDB_SECRET_NAME"]

secret_manager_client = secretmanager_v1.SecretManagerServiceClient()
secret_name = secret_manager_client.secret_version_path(project=PROJECT_ID, secret=ALLOYDB_SECRET_NAME, secret_version="latest")
secret_request = secretmanager_v1.AccessSecretVersionRequest(name=secret_name)
secret_response = secret_manager_client.access_secret_version(request=secret_request)
PGPASSWORD = secret_response.payload.data.decode("UTF-8").strip()

engine = AlloyDBEngine.from_instance(
    project_id=PROJECT_ID,
    region=REGION,
    cluster=ALLOYDB_CLUSTER_NAME,
    instance=ALLOYDB_INSTANCE_NAME,
    database=ALLOYDB_DATABASE_NAME,
    user="postgres",
    password=PGPASSWORD
)

# Create a synchronous connection to our vectorstore
vectorstore = AlloyDBVectorStore.create_sync(
    engine=engine,
    table_name=ALLOYDB_TABLE_NAME,
    embedding_service=GoogleGenerativeAIEmbeddings(model="models/embedding-001"),
    id_column="id",
    content_column="description",
    embedding_column="product_embedding",
    metadata_columns=["id", "name", "categories"]
)


def create_app():
    app = Flask(__name__)

    @app.route("/", methods=['POST'])
    def talkToGemini():
        print("Beginning RAG call")
        payload = request.get_json(silent=True) or {}

        # Bound and sanitize untrusted inputs at the HTTP boundary before
        # any prompt is constructed (R-020). Failure is HTTP 400, no LLM
        # call is made, and no untrusted bytes reach the embedding API.
        raw_message = payload.get('message')
        if isinstance(raw_message, str):
            raw_message = unquote(raw_message)
        try:
            prompt = sanitize_message(raw_message)
            image_url = sanitize_image_url(payload.get('image'))
        except SanitizationError as e:
            return jsonify({'error': str(e)}), 400

        # Step 1 - Get a room description from the vision model.
        # System instruction is structurally separate from the user-supplied
        # image URL.
        llm_vision = ChatGoogleGenerativeAI(model="gemini-1.5-flash")
        vision_messages = [
            SystemMessage(content=VISION_SYSTEM_PROMPT),
            HumanMessage(
                content=[
                    {"type": "image_url", "image_url": image_url},
                ]
            ),
        ]
        response = llm_vision.invoke(vision_messages)
        print("Description step:")
        print(response)
        description_response = response.content

        # Step 2 - Similarity search.
        # The vector query is used for embedding/retrieval, not as an LLM
        # instruction; injection here only degrades retrieval quality. We
        # still cap the size to protect the embedding API.
        vector_search_prompt = build_vector_search_query(prompt, description_response)
        print(vector_search_prompt)

        docs = vectorstore.similarity_search(vector_search_prompt)
        print(f"Vector search: {description_response}")
        print(f"Retrieved documents: {len(docs)}")
        relevant_docs = ""
        for doc in docs:
            doc_details = doc.to_json()
            print(f"Adding relevant document to prompt context: {doc_details}")
            relevant_docs += str(doc_details) + ", "

        # Step 3 - Final recommendation.
        # SystemMessage holds the immutable role and output format; the
        # HumanMessage carries the three labeled, untrusted sections. This
        # is the canonical structural defense for prompt injection.
        llm = ChatGoogleGenerativeAI(model="gemini-1.5-flash")
        design_messages = [
            SystemMessage(content=DESIGN_SYSTEM_PROMPT),
            build_design_human_message(prompt, description_response, relevant_docs),
        ]
        print("Final design messages assembled")
        design_response = llm.invoke(design_messages)

        data = {'content': design_response.content}
        return data

    return app

if __name__ == "__main__":
    # Create an instance of flask server when called directly
    app = create_app()
    app.run(host='0.0.0.0', port=8080)
