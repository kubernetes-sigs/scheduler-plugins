# test_substreams.py
import random, hashlib

BASE_SEED = 424242

def derive_seed(base_seed: int, *labels: object, nbytes: int = 16) -> int:
    h = hashlib.blake2b(digest_size=nbytes)
    h.update(int(base_seed).to_bytes(16, "big", signed=False))
    for lab in labels:
        h.update(b"\x00")
        h.update(str(lab).encode("utf-8"))
    return int.from_bytes(h.digest(), "big")

def gen_layout(change=False):
    rng = random.Random(derive_seed(BASE_SEED, "layout"))
    out = []
    for _ in range(12):
        if change:
            _ = rng.random()  # <-- simulate a refactor: extra random call
        out.append(rng.randint(1, 5))
    return out

def gen_cpu_parts():
    rng = random.Random(derive_seed(BASE_SEED, "cpu_parts"))
    return [rng.randint(100, 1500) for _ in range(12)]

def run(change=False):
    layout = gen_layout(change=change)
    cpu = gen_cpu_parts()  # independent RNG stream
    return layout, cpu

if __name__ == "__main__":
    base_layout, base_cpu = run(change=False)
    chg_layout, chg_cpu   = run(change=True)

    print("SUBSTREAMS (hash-separated)")
    print("layout_equal:", base_layout == chg_layout)
    print("cpu_equal   :", base_cpu == chg_cpu)
    print("sample_cpu_base :", base_cpu[:6])
    print("sample_cpu_changed:", chg_cpu[:6])
