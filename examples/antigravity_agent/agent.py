# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import asyncio
import sys
from google.antigravity import Agent, LocalAgentConfig

# Expose agent_config globally for harness_server.py loading
agent_config = LocalAgentConfig(
    system_instructions="You are a helpful assistant powered by Google Antigravity."
)

async def main():
    # Initialize the agent session using the global config.
    # It automatically picks up GEMINI_API_KEY from the environment.
    async with Agent(agent_config) as agent:
        prompt = sys.argv[1] if len(sys.argv) > 1 else "Explain quantum computing in one sentence."
        response = await agent.chat(prompt)
        print(await response.text())

if __name__ == "__main__":
    asyncio.run(main())
