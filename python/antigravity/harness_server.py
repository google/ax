# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# NOTE ON ARCHITECTURE:
# This gRPC server implements the AX HarnessService protocol. It embeds the
# Antigravity weather agent logic directly, serving it over production gRPC.

import argparse

import asyncio
import json
import logging
import os
import pathlib
import re
import sys
from typing import TypedDict
import grpc
from grpc_health.v1 import health, health_pb2, health_pb2_grpc
from google.protobuf.struct_pb2 import Struct


from python.proto import ax_pb2
from python.proto import ax_pb2_grpc
from python.proto import content_pb2
from google.antigravity import Agent, AgentConfig, LocalAgentConfig
from google.antigravity.types import Text, Thought, ToolCall

# Fields that come from outside harness_config and must not be set through it:
#   - conversation_id: taken from the runtime request (request.conversation_id).
#   - save_dir: derived at the server level from the configured state_dir.
# TODO: add validation for fields that are unsafe to set per execution
# (e.g. credentials, deployment routing) or that may only be set at
# conversation creation.
_NON_HARNESS_CONFIG_FIELDS = frozenset({"conversation_id", "save_dir"})


class HarnessConfigError(ValueError):
    """Raised when request harness_config is not a valid overlay."""


class VertexKwargs(TypedDict, total=False):
    """Typed subset of LocalAgentConfig kwargs needed to enable Vertex AI.

    `total=False` so {} is a valid value (returned when env does not request
    Vertex). When populated, all three keys are present.
    """

    vertex: bool
    project: str
    location: str


def _env_use_vertex() -> bool:
    """True if env requests the Vertex AI backend (vs. AI Studio API key)."""
    return os.environ.get("GOOGLE_GENAI_USE_VERTEXAI", "").lower() in (
        "true",
        "1",
    ) or os.environ.get("GOOGLE_GENAI_USE_ENTERPRISE", "").lower() in ("true", "1")


def _vertex_kwargs_from_env() -> VertexKwargs:
    """Returns LocalAgentConfig kwargs from GOOGLE_CLOUD_{PROJECT,LOCATION} env.

    Temporary override until AGY supports reading these env vars natively.
    Returns {} when env does not request Vertex (caller's programmatic config
    stands as-is). When env requests Vertex, returns VertexKwargs populated
    for passing to LocalAgentConfig.__init__ so AGY's @model_validator picks
    them up.

    Raises ValueError if env requests Vertex but project/location are missing.

    TODO: remove once AGY reads these env vars natively.
    """
    if not _env_use_vertex():
        return {}

    project = os.environ.get("GOOGLE_CLOUD_PROJECT", "")
    location = os.environ.get("GOOGLE_CLOUD_LOCATION", "")

    missing = [
        name
        for name, value in (
            ("project (set GOOGLE_CLOUD_PROJECT)", project),
            ("location (set GOOGLE_CLOUD_LOCATION)", location),
        )
        if not value
    ]
    if missing:
        raise ValueError(
            "Vertex AI backend requested but missing required config: "
            + ", ".join(missing)
        )

    print(f"Vertex AI backend configured: project={project} location={location}")
    return {"vertex": True, "project": project, "location": location}


def _build_default_config() -> LocalAgentConfig:
    """Builds the default agent config the sidecar serves on startup.

    Vertex configuration is read from env via `_vertex_kwargs_from_env`.

    TODO(#194): per-request `harness_config` will override fields of this
    default on a per-conversation basis. Until then, every conversation uses
    this config.
    """
    return LocalAgentConfig(
        system_instructions="You are a helpful agent.",
        **_vertex_kwargs_from_env(),
    )


def _has_credentials(config: AgentConfig | None) -> bool:
    """Checks if Gemini credentials are set per AGY's accepted sources.

    Mirrors AGY's own validation. AGY accepts exactly these sources:
      1. GEMINI_API_KEY environment variable (read directly by AGY).
      2. config.api_key set programmatically (AI Studio path).
      3. config.vertex=True + config.{project,location} set (Vertex path).
      4. config.vertex=True + config.api_key set (Vertex Express Mode;
         covered by case 2).

    Anything else (e.g. vertex=True without project/location) is rejected
    by AGY at request time, so we reject it here at startup too.
    """
    # Check env - AGY reads GEMINI_API_KEY directly from os.environ.
    if os.environ.get("GEMINI_API_KEY"):
        return True

    # Check passed in config
    if config:
        if getattr(config, "api_key", None):
            return True
        if getattr(config, "vertex", False):
            # Vertex requires project + location, unless an api_key (Express
            # Mode) is set - but Express Mode would have returned True above.
            if getattr(config, "project", None) and getattr(config, "location", None):
                return True

    return False


def _existing_sdk_conv_id(save_dir: str) -> str | None:
    # SDK persists each conversation as {save_dir}/{sdk_conv_id}.db where sdk_conv_id
    # is SDK-picked (a hash), not our AX conversation_id. We give each AX conversation
    # its own save_dir so at most one .db lives there; the SDK conv_id is its stem.
    dbs = list(pathlib.Path(save_dir).glob("*.db"))
    return dbs[0].stem if dbs else None


def _reject_disallowed_fields(overrides: dict[str, object]) -> None:
    """Best-effort validation of a request harness_config overlay's keys.

    Rejects fields managed outside harness_config and unknown top-level
    fields (typos that LocalAgentConfig's extra="ignore" would otherwise
    silently drop).
    Top-level only: nested-key and value/type validation is delegated to the
    SDK's own LocalAgentConfig validation when the config is constructed.
    """
    managed = sorted(set(overrides) & _NON_HARNESS_CONFIG_FIELDS)
    if managed:
        raise HarnessConfigError(
            f"field(s) managed outside harness_config cannot be set: {', '.join(managed)}"
        )
    unknown = sorted(set(overrides) - set(LocalAgentConfig.model_fields))
    if unknown:
        raise HarnessConfigError(f"unknown config field(s): {', '.join(unknown)}")


class AntigravityHarnessServiceServicer(ax_pb2_grpc.HarnessServiceServicer):
    """Implements the ax.HarnessService protocol over gRPC."""

    def __init__(self, default_config: AgentConfig, state_dir: pathlib.Path):
        self._default_config = default_config
        self._state_dir = state_dir

    def _build_config_for(
        self, conversation_id: str, harness_config: bytes = b""
    ) -> LocalAgentConfig:
        # Overlay the request's harness_config (JSON-in-bytes) onto the server
        # default. The parsed dict is a local intermediate only; this method's
        # boundary type is the validated LocalAgentConfig.
        if harness_config:
            try:
                overrides = json.loads(harness_config.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError) as exc:
                raise HarnessConfigError(f"expected UTF-8 JSON: {exc}") from exc
            if not isinstance(overrides, dict):
                raise HarnessConfigError("top-level JSON value must be an object")
            _reject_disallowed_fields(overrides)
        else:
            overrides = {}

        # Persistence values managed outside harness_config go on last so a
        # request can never redirect trajectory storage. Per-AX-conv save_dir
        # under the configured state_dir base; resume by the SDK's own conv_id
        # if a trajectory already exists there. SDK auto-creates the directory.
        overrides["save_dir"] = str(self._state_dir / conversation_id)
        if sdk_conv_id := _existing_sdk_conv_id(overrides["save_dir"]):
            overrides["conversation_id"] = sdk_conv_id

        # Reconstruct (not model_copy) so the SDK re-validates overlaid values
        # and surfaces its own error.
        values = {
            name: getattr(self._default_config, name)
            for name in self._default_config.model_fields_set
        }
        values.update(overrides)
        try:
            return LocalAgentConfig(**values)
        except (TypeError, ValueError) as exc:
            raise HarnessConfigError(str(exc)) from exc

    async def Connect(self, request_iterator, context):
        # Each HarnessRequest{start} drives one stateless turn; the stream stays
        # open across turns until the client half-closes.
        async for request in request_iterator:
            if request.WhichOneof("type") != "start":
                continue  # cancel frames not handled yet

            async for response in self._run_turn(request):
                yield response

    async def _run_turn(self, request):
        print(f"[gRPC] Connect turn requested. conv_id={request.conversation_id}")

        # 1. Retrieve and check messages
        ax_messages = request.start.messages
        if not ax_messages:
            yield ax_pb2.HarnessResponse(
                conversation_id=request.conversation_id,
                end=ax_pb2.HarnessEnd(
                    state=ax_pb2.STATE_FAILED,
                    error=ax_pb2.Error(
                        code=3,  # INVALID_ARGUMENT
                        description="No messages found in start payload",
                    ),
                ),
            )
            return

        latest_message = ax_messages[-1]

        if latest_message.content.WhichOneof("type") != "text":
            yield ax_pb2.HarnessResponse(
                conversation_id=request.conversation_id,
                end=ax_pb2.HarnessEnd(
                    state=ax_pb2.STATE_FAILED,
                    error=ax_pb2.Error(
                        code=3,  # INVALID_ARGUMENT
                        description="Latest message must contain text content",
                    ),
                ),
            )
            return
        latest_query_text = latest_message.content.text.text

        if not self._default_config:
            yield ax_pb2.HarnessResponse(
                conversation_id=request.conversation_id,
                end=ax_pb2.HarnessEnd(
                    state=ax_pb2.STATE_FAILED,
                    error=ax_pb2.Error(
                        code=9,  # FAILED_PRECONDITION
                        description="Agent config is not loaded on the server",
                    ),
                ),
            )
            return
        try:
            per_conv_config = self._build_config_for(
                request.conversation_id, request.start.harness_config
            )
            print(
                f"[gRPC] Starting Agent for conv_id={request.conversation_id}, save_dir={per_conv_config.save_dir}"
            )
            async with Agent(per_conv_config) as agent:
                conversation = agent.conversation

                print(f"[gRPC] Running chat query: {latest_query_text}")
                response = await conversation.chat(latest_query_text)

                # To avoid streaming individual tokens inside TextContent messages (which is not
                # supported by the Interactions proto/TextContent specifications), we buffer
                # contiguous blocks of text and thought chunks, yielding them only when the
                # contiguous block ends or a different chunk type is received.
                text_chunks = []
                thought_chunks = []

                def flush_text():
                    if not text_chunks:
                        return None
                    msg = ax_pb2.Message(
                        role="assistant",
                        content=content_pb2.Content(
                            text=content_pb2.TextContent(text="".join(text_chunks))
                        ),
                    )
                    text_chunks.clear()
                    return ax_pb2.HarnessResponse(
                        conversation_id=request.conversation_id,
                        outputs=ax_pb2.HarnessOutputs(messages=[msg]),
                    )

                def flush_thought():
                    if not thought_chunks:
                        return None

                    # Normalize Gemini's thinking output: collapse runs of 3+ newlines
                    # (emitted between reasoning sections) to exactly one blank line
                    # and strip trailing whitespace. Then append exactly one trailing
                    # newline so downstream displays render a blank line between the
                    # thinking block and whatever follows (next thought, tool call,
                    # or answer text). Without the trailing newline the display's
                    # transition logic emits only one newline, gluing blocks together.
                    raw_text = "".join(thought_chunks)
                    clean_text = re.sub(r"\n{3,}", "\n\n", raw_text).rstrip() + "\n"

                    summary = [
                        content_pb2.ThoughtSummaryContent(
                            text=content_pb2.TextContent(text=clean_text)
                        )
                    ]
                    thought_chunks.clear()
                    msg = ax_pb2.Message(
                        role="model",
                        content=content_pb2.Content(
                            thought=content_pb2.ThoughtContent(summary=summary)
                        ),
                    )
                    return ax_pb2.HarnessResponse(
                        conversation_id=request.conversation_id,
                        outputs=ax_pb2.HarnessOutputs(messages=[msg]),
                    )

                async for chunk in response.chunks:
                    if isinstance(chunk, Text):
                        if resp := flush_thought():
                            yield resp
                        text_chunks.append(chunk.text)
                    elif isinstance(chunk, Thought):
                        if resp := flush_text():
                            yield resp
                        thought_chunks.append(chunk.text)
                    elif isinstance(chunk, ToolCall):
                        # Flush all pending text/thought buffers before dispatching the tool call
                        if resp := flush_text():
                            yield resp
                        if resp := flush_thought():
                            yield resp

                        struct_args = Struct()
                        struct_args.update(chunk.args)

                        func_call = content_pb2.FunctionCallContent(
                            name=str(chunk.name), arguments=struct_args
                        )
                        msg = ax_pb2.Message(
                            role="model",
                            content=content_pb2.Content(
                                tool_call=content_pb2.ToolCallContent(
                                    id=chunk.id or "", function_call=func_call
                                )
                            ),
                        )
                        yield ax_pb2.HarnessResponse(
                            conversation_id=request.conversation_id,
                            outputs=ax_pb2.HarnessOutputs(messages=[msg]),
                        )

                # Flush any remaining text/thought buffers after the generator loop ends
                if resp := flush_text():
                    yield resp
                if resp := flush_thought():
                    yield resp

                # Yield completion end frame
                yield ax_pb2.HarnessResponse(
                    conversation_id=request.conversation_id,
                    end=ax_pb2.HarnessEnd(state=ax_pb2.STATE_COMPLETED),
                )
                print("[gRPC] Turn completed successfully.")

        except HarnessConfigError as exc:
            yield ax_pb2.HarnessResponse(
                conversation_id=request.conversation_id,
                end=ax_pb2.HarnessEnd(
                    state=ax_pb2.STATE_FAILED,
                    error=ax_pb2.Error(
                        code=3,  # INVALID_ARGUMENT
                        description=f"Invalid harness_config: {exc}",
                    ),
                ),
            )
            return
        except Exception as e:
            logging.exception("Error inside Connect servicer execution")
            yield ax_pb2.HarnessResponse(
                conversation_id=request.conversation_id,
                end=ax_pb2.HarnessEnd(
                    state=ax_pb2.STATE_FAILED,
                    error=ax_pb2.Error(
                        code=13,  # INTERNAL
                        description=f"Agent execution terminated due to error. ({str(e)})",
                    ),
                ),
            )
            return


async def _serve(
    host: str, port: int, default_config: AgentConfig, state_dir: pathlib.Path
):
    server = grpc.aio.server()
    servicer = AntigravityHarnessServiceServicer(default_config, state_dir)
    ax_pb2_grpc.add_HarnessServiceServicer_to_server(servicer, server)

    # Serve the standard gRPC health protocol.
    health_servicer = health.aio.HealthServicer()
    health_pb2_grpc.add_HealthServicer_to_server(health_servicer, server)
    await health_servicer.set("", health_pb2.HealthCheckResponse.SERVING)

    listen_addr = f"{host}:{port}"
    server.add_insecure_port(listen_addr)
    print(f"Starting gRPC harness server on {listen_addr}...")
    await server.start()
    await server.wait_for_termination()


def _enhance_config_from_env(config) -> None:
    skills_dir = os.environ.get("SKILLS_DIR")
    if skills_dir and os.path.isdir(skills_dir):
        print(f"Adding preinstalled skills directory to agent config: {skills_dir}")
        if not hasattr(config, "skills_paths") or config.skills_paths is None:
            config.skills_paths = []
        config.skills_paths = list(config.skills_paths)
        if skills_dir not in config.skills_paths:
            config.skills_paths.append(skills_dir)


def main():
    parser = argparse.ArgumentParser(description="Antigravity gRPC Harness Server")
    parser.add_argument(
        "--port", type=int, default=50053, help="Port to bind the server to"
    )
    parser.add_argument(
        "--host", default="localhost", help="Host to bind the server to"
    )
    parser.add_argument(
        "--state-dir",
        default=str(pathlib.Path.home() / ".ax" / "antigravity" / "conversations"),
        help="Base directory for per-conversation trajectory storage",
    )
    args = parser.parse_args()

    try:
        default_config = _build_default_config()
        _enhance_config_from_env(default_config)
        if not _has_credentials(default_config):
            raise ValueError(
                "No Gemini credentials configured. Set GEMINI_API_KEY "
                "(AI Studio) or GOOGLE_GENAI_USE_VERTEXAI=True + "
                "GOOGLE_CLOUD_{PROJECT,LOCATION} (Vertex AI)."
            )
    except ValueError as e:
        # Single startup-config exit point.
        print(f"ERROR: {e}", file=sys.stderr)
        sys.exit(1)

    asyncio.run(
        _serve(
            args.host,
            args.port,
            default_config,
            pathlib.Path(args.state_dir).expanduser(),
        )
    )


if __name__ == "__main__":
    main()
