# The export step (end of a training notebook)

You train the model however you like. When it is trained, one step gets the run folder
ready for promotion: it writes `training_manifest.json`, exports a GGUF next to the
safetensors, and runs the same file checks promotion runs, with a fix for anything
refused. Run it before you upload. The code is `intelligence/modelstore/export.py`
(`mhai-models export`).

## Before the step

Save the merged model (LoRA merged into the base) with safetensors into a local folder
such as `/content/run-1`. Keep it off Drive while you work, because the conversion reads
every weight:

```python
model.save_pretrained("/content/run-1", safe_serialization=True, max_shard_size="4GB")
tokenizer.save_pretrained("/content/run-1")
```

The datasets you name must already be in `models/approved-datasets.json` (synthetic only
for now), and an upstream base model's licence must be in `models/licences.json`
(Apache-2.0 or MIT today). Both lists change only through a pull request.

## The Colab cells

Setup, once per session. Clone the repository at the commit you were given, and pin
llama.cpp (`v1.0.0`, commit `b1b770d90d94cad9e9ea5b993bdbc383a522fb28`):

```python
!git clone https://github.com/Measured-Health/mhai-multiagent /content/mhai
!pip install --no-deps -e /content/mhai/services/intelligence
!git clone https://github.com/ggml-org/llama.cpp /content/llama.cpp
!cd /content/llama.cpp && git checkout b1b770d90d94cad9e9ea5b993bdbc383a522fb28
!pip install sentencepiece
!cmake -S /content/llama.cpp -B /content/llama.cpp/build -DLLAMA_CURL=OFF
!cmake --build /content/llama.cpp/build --target llama-quantize -j 4
```

`--no-deps` is deliberate: the step imports only the standard library, and Colab's own
torch stays as it is. Don't install llama.cpp's `requirements` file, which pins a CPU
torch. The converter uses the `gguf-py` package from the checkout.

The step itself goes at the end of your notebook. Change only the values:

```python
from intelligence.modelstore.export import ExportError, export_run, fix_for
from intelligence.modelstore.reasons import Refused

try:
    out = export_run(
        "/content/run-1",
        name="mhai-answer",                       # lowercase letters, digits, '-'
        version="0.1.0",                          # the version you will ask to promote
        role="cca",                               # the answer model
        datasets=["clinical-qa-synthetic@0.1.0"], # must be in models/approved-datasets.json
        trained_by="your-github-login",           # lowercase
        base_upstream="Qwen/Qwen2.5-1.5B-Instruct",
        base_revision="<the 40-character commit you trained from>",
        base_licence="Apache-2.0",
        # or, for a base we promoted: base_promoted="<name>@<version>#<sha256>"
        notebook_metrics={"train_loss": train_loss, "eval_loss": eval_loss},
        quant="q4_k_m",                           # or q5_k_m, q6_k, q8_0
        llama_cpp="/content/llama.cpp",
        datasets_registry="/content/mhai/models/approved-datasets.json",
        licences_registry="/content/mhai/models/licences.json",
    )
    print("passed; files hash for --expect-files:", out.files_sha256)
except Refused as e:
    print("refused:", e.reason, e.detail_code or "", "\nfix:", fix_for(e))
except ExportError as e:
    print("error:", e)
```

The same step from a shell:
`python -m intelligence.modelstore.export /content/run-1 --name mhai-answer --version 0.1.0 --dataset <id> --trained-by <login> --base-upstream <repo> --base-revision <sha> --base-licence Apache-2.0 --metric train_loss=0.42 --llama-cpp /content/llama.cpp --datasets /content/mhai/models/approved-datasets.json --licences /content/mhai/models/licences.json`.
If the step reports a mistake, fix it and run it again. It rewrites the manifest and the
GGUF each time.

## What the folder must hold

- `config.json`, `generation_config.json` and the tokenizer JSON files
- `*.safetensors` (or the index and its shards)
- one `model-<quant>.gguf`, written by this step
- `training_manifest.json`

Nothing else is allowed: no `.bin`, `.pt`, checkpoints, `README.md` or code. Tokenizer side
files (`vocab.json`, `merges.txt`, `tokenizer.model`) are needed by the converter, so
delete them after the step writes the GGUF. The step tells you which file to remove.

Don't name the training manifest `manifest.json`. That name belongs to the signed store
manifest that verification trusts, so a run holding one is refused with
`training_manifest_misnamed`.

## After the step

Upload the folder to the shared Drive folder as `models/<name>/<run>/` (for example
`models/mhai-answer/run-1/`). Then ask for the promotion with the line the step printed:
`mhai-models promote <name> <version> --source drive --run <run> --expect-files <hash>`.
A `cca` run passes these checks today but can't be pinned until its evaluation suite
lands (own answer model plan, PR 2).
