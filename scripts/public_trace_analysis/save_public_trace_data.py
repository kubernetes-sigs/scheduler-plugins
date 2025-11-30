#!/usr/bin/env python3
# save_public_trace_data.py

import csv, os
from typing import List, Tuple
from google.cloud import bigquery

# ----------------------------------------------------------------------
# Configuration
# ----------------------------------------------------------------------

ALIBABA_IN = "alibaba-clusterdata/cluster-trace-gpu-v2025/disaggregated_DLRM_trace.csv"
ALIBABA_OUT = "alibaba-clusterdata/cluster-trace-gpu-v2025/data.csv"

GOOGLE_OUT = "google-cluster-data/ClusterData2019/data.csv"
GOOGLE_PROJECT_ID = "master-thesis-479005"
GOOGLE_CELL_DATASETS = [
    "clusterdata_2019_a",
    "clusterdata_2019_b",
    "clusterdata_2019_c",
    "clusterdata_2019_d",
    "clusterdata_2019_e",
    "clusterdata_2019_f",
    "clusterdata_2019_g",
    "clusterdata_2019_h",
]
GOOGLE_TARGET_TOTAL = 100_000

# ----------------------------------------------------------------------
# Types
# ----------------------------------------------------------------------

# (id, created_time_s, cpu_request, mem_request, life_time_s, priority)
Row = Tuple[str, float, float, float, float, int | None]

# ----------------------------------------------------------------------
# Alibaba loader
# ----------------------------------------------------------------------

def load_alibaba_rows(path: str) -> List[Row]:
    """
    Alibaba disaggregated_DLRM_trace.csv:
    instance_sn, cpu_request, memory_request, creation_time, deletion_time
    Priority is not available → stored as None.
    """
    rows: List[Row] = []

    with open(path, "r", newline="") as f:
        reader = csv.DictReader(f)
        for row in reader:
            inst = (row.get("instance_sn") or "").strip()
            cpu_raw = (row.get("cpu_request") or "").strip()
            mem_raw = (row.get("memory_request") or "").strip()
            ct_raw = (row.get("creation_time") or "").strip()
            dt_raw = (row.get("deletion_time") or "").strip()

            if not inst or not (cpu_raw and mem_raw and ct_raw and dt_raw):
                continue

            cpu_req = float(cpu_raw)
            mem_req = float(mem_raw)
            created_s = float(ct_raw)
            deleted_s = float(dt_raw)
            life_time_s = deleted_s - created_s

            rows.append((inst, created_s, cpu_req, mem_req, life_time_s, None))

    return rows


# ----------------------------------------------------------------------
# Google Cluster 2019 loader (via BigQuery)
# ----------------------------------------------------------------------

def load_google_rows(
    project_id: str,
    cell_datasets: list[str],
    target_total: int,
) -> List[Row]:
    """
    Sample instances from Google Cluster 2019 via BigQuery.
    For each instance, we return:
    id, cpu_request, mem_request, inter_s, life_time_s, priority
    Where:
      - id          = "{cell_dataset}:{collection_id}:{instance_index}"
      - inter_s     = true inter-arrival time (µs → s) to previous instance
      - life_time_s = (max(end_time) - min(start_time)) over usage windows (µs → s)
      - priority    = priority from first event
    """
    client = bigquery.Client(project=project_id)
    target_per_cell = target_total // max(len(cell_datasets), 1)

    rows: List[Row] = []

    for dataset in cell_datasets:
        print(f"Sampling from {dataset} ...")

        query = f"""
        WITH first_events AS (
          SELECT
            collection_id,
            instance_index,
            time AS start_time_us,
            resource_request.cpus   AS cpu_request,
            resource_request.memory AS mem_request,
            priority,
            ROW_NUMBER() OVER (
              PARTITION BY collection_id, instance_index
              ORDER BY time
            ) AS rn
          FROM `google.com:google-cluster-data.{dataset}.instance_events`
          WHERE time > 0
            AND resource_request IS NOT NULL
            AND resource_request.cpus   > 0
            AND resource_request.cpus   <= 1.0
            AND resource_request.memory > 0
            AND resource_request.memory <= 1.0
        ),

        first_per_instance AS (
          SELECT
            collection_id,
            instance_index,
            start_time_us,
            cpu_request,
            mem_request,
            priority
          FROM first_events
          WHERE rn = 1
        ),

        with_deltas AS (
          SELECT
            collection_id,
            instance_index,
            start_time_us,
            cpu_request,
            mem_request,
            priority,
            start_time_us - LAG(start_time_us) OVER (ORDER BY start_time_us) AS inter_arrival_us
          FROM first_per_instance
        ),

        usage_agg AS (
          SELECT
            collection_id,
            instance_index,
            MIN(start_time) AS usage_start_us,
            MAX(end_time)   AS usage_end_us
          FROM `google.com:google-cluster-data.{dataset}.instance_usage`
          WHERE start_time > 0
            AND end_time IS NOT NULL
            AND end_time > start_time
          GROUP BY collection_id, instance_index
        ),

        joined AS (
          SELECT
            "{dataset}" AS cell_dataset,
            u.collection_id,
            u.instance_index,
            w.start_time_us,
            (u.usage_end_us - u.usage_start_us) AS life_time_us,
            w.inter_arrival_us,
            w.cpu_request,
            w.mem_request,
            w.priority
          FROM with_deltas w
          JOIN usage_agg u
            USING (collection_id, instance_index)
          WHERE (u.usage_end_us - u.usage_start_us) > 0
            AND w.inter_arrival_us IS NOT NULL
        )

        SELECT
          cell_dataset,
          collection_id,
          instance_index,
          start_time_us,
          life_time_us,
          inter_arrival_us,
          cpu_request,
          mem_request,
          priority
        FROM joined
        ORDER BY RAND()
        LIMIT {target_per_cell}
        """

        job = client.query(query)

        cell_rows: List[Row] = []
        for row in job:
            cell_dataset   = row["cell_dataset"]
            collection_id  = row["collection_id"]
            instance_index = row["instance_index"]

            inst_id     = f"{cell_dataset}:{collection_id}:{instance_index}"
            life_time_s = float(row["life_time_us"]) / 1_000_000.0
            inter_s     = float(row["inter_arrival_us"]) / 1_000_000.0
            cpu_req     = float(row["cpu_request"])
            mem_req     = float(row["mem_request"])
            priority    = int(row["priority"])

            cell_rows.append((inst_id, cpu_req, mem_req, inter_s, life_time_s, priority))

        print(f"  got {len(cell_rows)} rows")
        rows.extend(cell_rows)

    total = len(rows)
    print(f"Total sampled instances (before trim): {total}")

    if total > target_total:
        rows  = rows[:target_total]
        total = target_total
        print(f"Trimmed to target_total = {target_total}")

    print(f"Using {total} sampled instances")
    return rows


# ----------------------------------------------------------------------
# Common processing
# ----------------------------------------------------------------------

def add_inter_arrivals(rows: List[Row]):
    """
    Given rows without inter-arrival times, compute and add them.
    Returns: (id, cpu_request, mem_request, inter_s, life_time_s, priority)
    """
    rows_sorted = sorted(rows, key=lambda r: r[1])
    converted = []
    prev_created_s: float | None = None
    for inst, created_s, cpu_req, mem_req, life_time_s, prio in rows_sorted:
        if prev_created_s is None:
            inter_s = 0.0
        else:
            inter_s = float(created_s - prev_created_s)
        prev_created_s = created_s
        converted.append((inst, cpu_req, mem_req, inter_s, life_time_s, prio))
    return converted


def write_output(path: str, rows, include_priority: bool) -> None:
    """
    Write output CSV with header:
    id,cpu_request,mem_request,inter_arrival_us,life_time_us[,priority]
    """
    dirpath = os.path.dirname(path)
    if dirpath:
        os.makedirs(dirpath, exist_ok=True)
    with open(path, "w", newline="") as f:
        writer = csv.writer(f)
        header = ["id", "cpu_request", "mem_request", "inter_arrival_us", "life_time_us"]
        if include_priority:
            header.append("priority")
        writer.writerow(header)
        for inst, cpu_req, mem_req, inter_s, life_time_s, prio in rows:
            inter_us = int(inter_s * 1_000_000)
            life_us  = int(life_time_s * 1_000_000)
            base_row = [inst, float(cpu_req), float(mem_req), inter_us, life_us]
            if include_priority:
                base_row.append(int(prio) if prio is not None else "")
            writer.writerow(base_row)

# ----------------------------------------------------------------------
# Main
# ----------------------------------------------------------------------

def main() -> None:
    
    # Alibaba
    print("=== Processing Alibaba GPU Cluster 2025 ===")
    print(f"Input : {ALIBABA_IN}")
    raw_rows = load_alibaba_rows(ALIBABA_IN)
    print(f"Loaded {len(raw_rows)} Alibaba rows")
    rows_with_inter = add_inter_arrivals(raw_rows)
    write_output(ALIBABA_OUT, rows_with_inter, include_priority=False)
    print(f"Wrote Alibaba output to {ALIBABA_OUT}")

    # Google
    print("=== Processing Google Cluster 2019 ===")
    print(f"Cells: {', '.join(GOOGLE_CELL_DATASETS)}")
    raw_rows = load_google_rows(
        project_id=GOOGLE_PROJECT_ID,
        cell_datasets=GOOGLE_CELL_DATASETS,
        target_total=GOOGLE_TARGET_TOTAL,
    )
    print(f"Loaded {len(raw_rows)} Google rows")
    write_output(GOOGLE_OUT, raw_rows, include_priority=True)
    print(f"Wrote Google output to {GOOGLE_OUT}")

if __name__ == "__main__":
    main()
