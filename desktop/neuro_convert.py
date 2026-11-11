#!/usr/bin/env python3
# Neuro-движок VoiceChanger: офлайн-конверсия тембра записи по голосу-референсу.
# Режимы: speech (seed-vc v1), song (seed-vc v2 — сохраняет мелодию записи).
# Все модели скачиваются один раз с HuggingFace в <engine>/hf-cache.
import os, sys, glob, shutil, runpy

HERE = os.path.dirname(os.path.abspath(__file__))

def parse():
    a = {"source": None, "reference": None, "output": None, "mode": "speech",
         "steps": 25, "shift": 0, "cache": os.path.join(HERE, "hf-cache")}
    argv, i = sys.argv[1:], 0
    while i < len(argv):
        k = argv[i].lstrip("-")
        if k in ("source", "reference", "output", "mode", "cache"):
            a[k] = argv[i + 1]; i += 2
        elif k in ("steps", "shift"):
            v = argv[i + 1]
            a[k] = v if (k == "shift" and v == "auto") else int(v); i += 2
        else:
            i += 1
    return a

def main():
    a = parse()
    os.environ.setdefault("HF_HOME", a["cache"])
    os.environ.setdefault("HF_HUB_DISABLE_TELEMETRY", "1")
    os.environ.setdefault("TRANSFORMERS_NO_ADVISORY_WARNINGS", "1")
    os.environ.setdefault("PYTHONWARNINGS", "ignore")
    if not a["source"] or not a["reference"] or not a["output"]:
        print("ERR нужны --source, --reference, --output"); sys.exit(2)

    import torch, torchaudio, soundfile as sf
    cuda = torch.cuda.is_available()
    print("PROGRESS device=" + ("cuda" if cuda else "cpu"), flush=True)

    # torchaudio 2.11 требует torchcodec для save — заменяем на soundfile
    def _save(path, src, sr, *args, **kw):
        if not torch.is_tensor(src):
            src = torch.as_tensor(src)
        src = src.detach().float().cpu()
        if src.dim() == 2 and src.shape[0] < src.shape[1]:
            src = src.T
        sf.write(str(path), src.numpy(), int(sr))
    torchaudio.save = _save

    tmpdir = os.path.join(HERE, "_tmp_out")
    shutil.rmtree(tmpdir, ignore_errors=True)
    os.makedirs(tmpdir, exist_ok=True)

    if a["mode"] == "song":
        args = ["--source", a["source"], "--target", a["reference"],
                "--output", tmpdir, "--diffusion-steps", str(a["steps"])]
        os.environ.setdefault("OMP_NUM_THREADS", "4")
        import seed_vc.inference_v2 as m  # noqa
        sys.argv = ["seed-vc-infer-v2"] + args + ([] if cuda else ["--compile", "False"])
        print("PROGRESS загрузка моделей song (первый раз — до 3 ГБ, затем из кэша)", flush=True)
    else:
        args = ["--source", a["source"], "--target", a["reference"],
                "--output", tmpdir, "--diffusion-steps", str(a["steps"])]
        if a["shift"] == "auto" or isinstance(a["shift"], int) and a["shift"]:
            args += ["--auto-f0-adjust", "True"]
        if isinstance(a["shift"], int) and a["shift"]:
            args += ["--semi-tone-shift", str(a["shift"])]
        fp16 = "True" if cuda else "False"
        args += ["--fp16", fp16]
        sys.argv = ["seed-vc-infer-v1"] + args
        print("PROGRESS загрузка моделей speech (первый раз — около 2 ГБ, затем из кэша)", flush=True)

    try:
        runpy.run_module("seed_vc.inference" if a["mode"] == "speech" else "seed_vc.inference_v2",
                         run_name="__main__")
    except SystemExit as e:
        if e.code not in (0, None):
            print("ERR inference code=" + str(e.code)); sys.exit(1)

    produced = sorted(glob.glob(os.path.join(tmpdir, "vc_*.wav")))
    if not produced:
        produced = [p for p in sorted(glob.glob(os.path.join(tmpdir, "*.wav")))]
    if not produced:
        print("ERR конверсия не создала файл"); sys.exit(1)
    out_abs = os.path.abspath(a["output"])
    os.makedirs(os.path.dirname(out_abs) or ".", exist_ok=True)
    shutil.move(produced[0], out_abs)
    shutil.rmtree(tmpdir, ignore_errors=True)
    info = sf.info(out_abs)
    print("OK {} | {} сек | {} Гц | {} кан".format(
        out_abs, round(info.duration, 1), info.samplerate, info.channels), flush=True)

if __name__ == "__main__":
    main()
