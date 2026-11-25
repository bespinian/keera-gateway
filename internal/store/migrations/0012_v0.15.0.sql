-- What v0.15.0 changes in the schema.

-- What a model can do beyond text: 'tools' for one that calls tools, and
-- 'vision' for one that reads images. A gate whose model reads images is
-- shown them too.
ALTER TABLE models ADD COLUMN capabilities text[] NOT NULL DEFAULT '{}';

-- Existing models get what the provider table in internal/catalog says, as a
-- new one would. Every chat model at Anthropic, OpenAI and Google does both.
UPDATE models SET capabilities = '{tools,vision}'
    WHERE kind = 'chat' AND provider IN ('anthropic', 'openai', 'google');
UPDATE models SET capabilities = known.capabilities
    FROM (VALUES
        ('deepseek', 'deepseek-v4-pro', '{tools}'::text[]),
        ('deepseek', 'deepseek-flash', '{tools,vision}'),
        ('mistral', 'mistral-large-4', '{tools,vision}'),
        ('mistral', 'mistral-small-2603', '{tools,vision}'),
        ('mistral', 'codestral-2508', '{tools}'),
        ('mistral', 'zai-glm-5-3', '{tools}'),
        ('mistral', 'zai-glm-5-3-flash', '{tools,vision}'),
        ('mistral', 'deepseek-v4-1-flash-1017', '{tools,vision}'),
        ('mistral', 'ministral-14b-2512', '{tools,vision}'),
        ('mistral', 'ministral-8b-2512', '{tools,vision}'),
        ('mistral', 'ministral-3b-2512', '{tools,vision}'),
        ('peakprivacy', 'mistral-small-swiss', '{tools}'),
        ('peakprivacy', 'glm5', '{tools}'),
        ('peakprivacy', 'deepseek-v4-flash', '{tools}'),
        ('peakprivacy', 'gpt-eu', '{tools}'),
        ('peakprivacy', 'deepseek-r1', '{tools}'),
        ('peakprivacy', 'gemma-3', '{vision}'),
        ('peakprivacy', 'qwen-3-fast', '{tools}'),
        ('peakprivacy', 'qwen-3', '{tools}'),
        ('infomaniak', 'Qwen/Qwen3.5-397B-A17B-FP8', '{tools,vision}'),
        ('infomaniak', 'moonshotai/Kimi-K2.6', '{tools,vision}'),
        ('infomaniak', 'mistralai/Mistral-Small-4-119B-2603', '{tools,vision}'),
        ('infomaniak', 'swiss-ai/Apertus-v1.5-70B', '{tools,vision}'),
        ('infomaniak', 'google/gemma-4-31B-it', '{tools}'),
        ('infomaniak', 'Qwen/Qwen3.8-27B-FP8', '{tools,vision}'),
        ('infomaniak', 'mistralai/Ministral-3-14B-Instruct-2512', '{tools,vision}'),
        ('stepping-stone', 'MiniMaxAI/MiniMax-M2.5', '{tools}'),
        ('stepping-stone', 'NVIDIA/NVIDIA-Nemotron-3-Super-120B-A12B', '{tools}'),
        ('stepping-stone', 'deepseek-ai/DeepSeek-V4-Flash-0731', '{tools}'),
        ('stepping-stone', 'zai-org/GLM-5.3-Flash', '{tools,vision}'),
        ('stepping-stone', 'openai/gpt-oss-120b', '{tools}'),
        ('stepping-stone', 'Qwen/Qwen3.8-27B', '{vision}'),
        ('stepping-stone', 'google/gemma-4-31B-it', '{tools,vision}'),
        ('stepping-stone', 'apertus-ai/Apertus-v1.5-8B', '{tools}'),
        ('stepping-stone', 'allenai/olmOCR-2-7B', '{vision}'),
        ('stepping-stone', 'lightonai/LightOnOCR-2-1B', '{vision}'),
        ('stepping-stone', 'opendatalab/MinerU2.5-2509-1.2B', '{vision}'),
        ('phoeniqs', 'inference-glm5', '{tools}'),
        ('phoeniqs', 'inference-qwen3-vl-235b', '{tools,vision}'),
        ('phoeniqs', 'inference-glm5-flash', '{tools}'),
        ('phoeniqs', 'inference-glm5-flash-compact', '{tools}'),
        ('phoeniqs', 'inference-apertus-v15-70b', '{tools,vision}'),
        ('phoeniqs', 'inference-llama4-maverick', '{vision}'),
        ('phoeniqs', 'inference-gemma4-31b', '{vision}'),
        ('phoeniqs', 'inference-llama4-scout-17b', '{vision}'),
        ('phoeniqs', 'inference-granite-vision-2b', '{vision}'),
        ('phoeniqs', 'inference-deepseek-ocr', '{vision}'),
        ('phoeniqs', 'inference-miner-u25', '{vision}'),
        ('cscs', 'zai-org/GLM-5.3-Flash', '{tools,vision}'),
        ('cscs', 'Qwen/Qwen3.8-Flash-Next-FP8', '{vision}'),
        ('cscs', 'google/gemma-4-31B-it', '{vision}'),
        ('cscs', 'Qwen/Qwen3.8-27B-FP8', '{vision}'),
        ('cscs', 'swiss-ai/Apertus-v1.5-70B', '{vision}'),
        ('cscs', 'swiss-ai/Apertus-v1.5-70B-thinking', '{vision}'),
        ('cscs', 'swiss-ai/Apertus-v1.5-8B', '{vision}'),
        ('cscs', 'swiss-ai/Apertus-v1.5-8B-thinking', '{vision}')
    ) AS known (provider, backend_model, capabilities)
    WHERE models.kind = 'chat' AND models.provider = known.provider
        AND models.backend_model = known.backend_model;

-- The operator key a sign-in was made with, sealed with KEERA_SECRET_KEY, so
-- changing the key ends its sessions, and the tokens Keera Chat and Keera
-- Agents were handed for them. Empty for everyone else. Sign-ins made with the
-- key before this have none and end now.
ALTER TABLE sessions ADD COLUMN operator_key bytea;
ALTER TABLE cli_codes ADD COLUMN operator_key bytea;
ALTER TABLE cli_tokens ADD COLUMN operator_key bytea;
