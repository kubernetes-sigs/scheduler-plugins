# Public Trace Analysis

To scripts has been provided for analyzing the two public traces we consider using for our workload generation: <[Alibaba GPU cluster trace v2025](https://github.com/alibaba/clusterdata/tree/master/cluster-trace-gpu-v2025)> and <[Google Borg trace 2019](https://github.com/google/cluster-data/blob/master/ClusterData2019.md)>.

- `save_data.py`: Script for extracting relevant fields from the raw traces and saving them into smaller CSV files for analysis.
- `plots.py`: Script for generating plots from the generated CSV files, including fitting Pareto Type I distributions.

First, run `save_data.py` to extract the data:

```bash
python3 save_data.py
```

Then, run `plots.py` to generate the combined grid plot:

```bash
python3 plots.py
```

## Pareto Type I distribution

The Pareto Type I distribution is defined by two parameters: a shape parameter ($\alpha > 0$) and a scale parameter $x_{\min} > 0$, which acts as a strict lower bound on the support. Its probability density function (PDF) is

$$
f(x; \alpha, x_{\min})
= \alpha \, \frac{x_{\min}^\alpha}{x^{\alpha + 1}},
\quad x \geq x_{\min}.
$$

The parameter $\alpha$ controls the heaviness of the tail, while $x_{\min}$ encodes the smallest plausible value (for example, a minimum CPU or memory request). The simplicity of Pareto Type I distribution make it attractive for our setting which also exhibits heavy-tailed behaviour, and for some cases want to enforce a lower bound $x_{\min}$ on sampled values.

We experimented with alternative distributions such as the Lomax distribution, which is a shifted version of the Pareto Type I distribution allowing for values in $[0, \infty)$, however, this introduced additional complexity in tuning. We also considered the log-normal distribution, which again was too complex to tune. Other distributions we explored include the exponential distribution, and zero-inflated models, but the Pareto Type I distribution remained a reasonable fit across all our considered datasets and resource types.

## Google Borg Trace 2019

Contains eight cells, which can be viewed as a cluster, with around 10,000 machines (nodes) each.

Group by cells when calculating arrival distributions across all cells.

We need to think of a way to get a representative distribution across all cells locally on the machine without storing all the data.

We are approximating the end time based on instance_usage table, and since this takes multiple traces we cannot say for sure that an instance has ended on that estiamation.

We discard instances that are already running

## Alibaba GPU Cluster Trace v2025

## Notes

We also considered using the <[Azure Public Dataset V2 trace](https://github.com/Azure/AzurePublicDataset/blob/master/AzurePublicDatasetV2.md)>, but it lacks important information such as priorities and host resources, making it less suitable for our analysis. Moreover, all VMs in the trace are created with a fixed time difference, which prevents us from modeling arrival distributions. Therefore, we decided not to use this dataset in our analysis.
