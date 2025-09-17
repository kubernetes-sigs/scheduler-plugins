# test_global_rng.py
import random

BASE_SEED = 424242

def gen_layout(rng, change=False):
    out = []
    for _ in range(12):
        if change:
            _ = rng.random()  # <-- simulate a refactor: extra random call
        out.append(rng.randint(1, 5))
    return out

def gen_cpu_parts(rng):
    # 12 CPU parts in [100, 1500]
    return [rng.randint(100, 1500) for _ in range(12)]

def run(change=False):
    rng = random.Random(BASE_SEED)
    layout = gen_layout(rng, change=change)
    cpu = gen_cpu_parts(rng)
    return layout, cpu

if __name__ == "__main__":
    base_layout, base_cpu = run(change=False)
    chg_layout, chg_cpu   = run(change=True)

    print("GLOBAL RNG")
    print("layout_equal:", base_layout == chg_layout)
    print("cpu_equal   :", base_cpu == chg_cpu)
    print("sample_cpu_base :", base_cpu[:6])
    print("sample_cpu_changed:", chg_cpu[:6])
