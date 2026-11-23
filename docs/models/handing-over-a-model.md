# Handing over a model for the GPU server

For the AI researcher. When a retrained answer model (`cca`) is ready, hand over **one
folder**; the operator puts it on the GPU server with one command
(`deploy/gpu-model/swap-model.sh`, deploy/README.md, "Swapping our own model"). We build
the GGUF ourselves, test it against our `clinical_answer` gate on the server, and serve it
only if it passes.

## What the folder holds

The QLoRA adapter as PEFT saves it, the tokenizer, and a training manifest. Nothing else:

| File | Required | Notes |
|---|---|---|
| `adapter_config.json` | yes | `peft_type` `LORA`; `base_model_name_or_path` exactly `google/gemma-4-E4B-it` |
| `adapter_model.safetensors` | yes | safetensors only |
| `tokenizer.json`, `tokenizer_config.json` | yes | `tokenizer.save_pretrained(folder)` |
| `training_manifest.json` | yes | below |
| `chat_template.jinja` or `chat_template.json` | if your tokenizer saves one | the template the model was trained with; it ends up in the GGUF |
| `special_tokens_map.json`, `tokenizer.model`, `added_tokens.json`, `README.md` | optional | |

Refused: any `.bin`, `.pt`, `.pth`, `.pkl`, `.joblib`, `.npy` (pickle), any `.py`,
`.sh`, `.ipynb` (code), checkpoints or other sub-folders, links, and any other file.
Nothing in the folder is ever loaded as code: no `auto_map`, no `trust_remote_code`.

## The base model

Train on `google/gemma-4-E4B-it` at revision
`4ecc2ba63bcac23ba4a767fc87025b1450f84c9c` (Apache-2.0). That is the only base the swap
accepts today; another base, or another revision of this one, is a pull request to
`BASES` in `services/intelligence/intelligence/modelstore/swap.py` (and its licence must be
in `models/licences.json`).

## The training manifest

`training_manifest.json`, schema `mhai.models.training/1`, as in
[`training_manifest.example.json`](training_manifest.example.json), with:

- `role`: `cca`;
- `base_model`: `{"upstream": "google/gemma-4-E4B-it", "revision":
  "4ecc2ba63bcac23ba4a767fc87025b1450f84c9c", "licence": "Apache-2.0"}`;
- `datasets`: ids from `models/approved-datasets.json` (a new dataset is a pull request
  there first: synthetic, or public with no patient data);
- `trained_by` (your GitHub login, lowercase), `framework` (package versions, such as
  `peft`, `transformers`, `torch`), `notebook_metrics` (numbers only), `created_at`.

## Tell the operator

The model's name (for example `gemma4-e4b-cca`), the new version (above the one served),
and the answer profile it was trained for (`gemma4-e4b-cca-v1` is the `steps-v1` prompt
of the first model; a new prompt format is a code change, so say so early).

## What happens next

1. The folder is checked as above, the base is merged with your adapter (PEFT
   `merge_and_unload`), converted with llama.cpp `b1b770d9` to f16 and quantised to
   `Q4_K_M`.
2. A second copy of the model service serves it beside the live one, and the
   `clinical_answer` gate asks every question (escalation recall 1.0, unsafe rate 0,
   grounded pass rate at least 0.70, fact coverage at least 0.70, over-escalation at most
   0.30; the retraining guidance in
   [the first model's intake](intake-2026-11-14-gemma4-e4b-cca-v1.md) still applies).
3. On a pass it is switched in; the previous model stays one command away (`--rollback`).
   The verdict and metrics are in `/srv/mhai-models/swaps.log`.
