/*
 * Copyright (C) 2006-2015 Michael Roitzsch <mroi@os.inf.tu-dresden.de>
 * economic rights: Technische Universitaet Dresden (Germany)
 */

#include <stddef.h>

/* An online updating solver for Linear Least Squares Problems.
 * Uses automatic stabilization by dropping columns to prevent overfitting.
 *
 * Typical use:
 * 
 * setup:
 *     llsp_t *solver = llsp_new(count);
 * add knowledge:
 *     llsp_add(solver, metrics, target_value);
 *     (void)llsp_solve(solver);
 * obtain a prediction:
 *     prediction = llsp_predict(solver, metrics);
 * tear down:
 *     llsp_dispose(solver);
 */

/* The running solution can be made to age out previously acquired knowledge
 * over time. When this aging factor is set to 0, no aging is performed. This
 * means the solution will be equivalent to a LLS-solution over all previous
 * metrics and target values. With an aging factor above zero, the solution will
 * slightly lean towards newly added metrics/targets, exhibting properties of a
 * sliding average. */
#ifndef AGING_FACTOR
#define AGING_FACTOR 0.01
#endif

/* During the column dropping check, each column's contribution to the accuracy
 * of the result is analyzed. An individual column must improve the accuracy by
 * at least this factor, otherwise it is considered a rank-deficiency and is
 * dropped. Values above 1.0 enable dropping. Higher values drop more, leading
 * to more stable, but less precise predictions. Set this to 0 to reliably
 * prevent dropping entirely. */
#ifndef COLUMN_CONTRIBUTION
#define COLUMN_CONTRIBUTION 1.1
#endif

/* an opaque handle for the LLSP solver/predictor */
typedef struct llsp_s llsp_t;

/* Allocates a new LLSP handle with the given number of metrics. */
llsp_t *llsp_new(size_t count);

/* This function adds another tuple of (metrics, measured target value) to the
 * LLSP solution. The metrics array must have as many values as stated in count
 * passed to llsp_new(). */
void llsp_add(llsp_t *restrict llsp, const double *restrict metrics, double target);

/* Solves the LLSP and returns a pointer to the resulting coefficients or NULL
 * if the training phase could not be successfully finalized. The pointer
 * remains valid until the LLSP context is freed. */
const double *llsp_solve(llsp_t *restrict llsp);

/* Predicts the target value from the given metrics. The context has to be
 * populated with a set of prediction coefficients by running llsp_solve(). */
double llsp_predict(llsp_t *restrict llsp, const double *restrict metrics);

/* Frees the LLSP context. */
void llsp_dispose(llsp_t *restrict llsp);
