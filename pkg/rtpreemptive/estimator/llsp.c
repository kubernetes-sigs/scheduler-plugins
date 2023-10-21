/*
 * Copyright (C) 2006-2015 Michael Roitzsch <mroi@os.inf.tu-dresden.de>
 * economic rights: Technische Universitaet Dresden (Germany)
 */

#include <stdbool.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include <math.h>
#include <assert.h>

#include "llsp.h"

#pragma clang diagnostic ignored "-Wvla"

/* float values below this are considered to be 0 */
#define EPSILON 1E-10


struct matrix {
	double **matrix;         // pointers to matrix data, indexed columns first, rows second
	size_t   columns;        // column count
};

struct llsp_s {
	size_t        metrics;   // metrics count
	double       *data;      // pointer to the malloc'ed data block, matrix is transposed
	struct matrix full;      // pointers to the matrix in its original form with all columns
	struct matrix sort;      // matrix with to-be-dropped columns shuffled to the right
	struct matrix good;      // reduced matrix with low-contribution columns dropped
	double        last_measured;
	double        result[];  // the resulting coefficients
};

static void givens_fixup(struct matrix m, size_t row, size_t column);
static void stabilize(struct matrix *sort, struct matrix *good);
static void trisolve(struct matrix m);

#pragma mark -


#pragma mark LLSP API Functions

llsp_t *llsp_new(size_t count)
{
	llsp_t *llsp;
	
	if (count < 1) return NULL;
	
	size_t llsp_size = sizeof(llsp_t) + count * sizeof(double);  // extra room for coefficients
	llsp = malloc(llsp_size);
	if (!llsp) return NULL;
	memset(llsp, 0, llsp_size);
	
	llsp->metrics = count;
	llsp->full.columns = count + 1;
	llsp->sort.columns = count + 1;
	
	return llsp;
}

void llsp_add(llsp_t *restrict llsp, const double *restrict metrics, double target)
{
	const size_t column_count = llsp->full.columns;
	const size_t row_count = llsp->full.columns + 1;  // extra row for shifting down and trisolve
	const size_t column_size = row_count * sizeof(double);
	const size_t data_size = column_count * row_count * sizeof(double);
	const size_t matrix_size = column_count * sizeof(double *);
	const size_t index_last = column_count - 1;
	
	if (!llsp->data) {
		llsp->data        = malloc(data_size);
		llsp->full.matrix = malloc(matrix_size);
		llsp->sort.matrix = malloc(matrix_size);
		llsp->good.matrix = malloc(matrix_size);
		if (!llsp->data || !llsp->full.matrix || !llsp->sort.matrix || !llsp->good.matrix)
			abort();
		
		for (size_t column = 0; column < llsp->full.columns; column++)
			llsp->full.matrix[column] =
			llsp->sort.matrix[column] = llsp->data + column * row_count;
		
		/* we need an extra column for the column dropping scan */
		llsp->good.matrix[index_last] = malloc(column_size);
		if (!llsp->good.matrix[index_last]) abort();
		
		memset(llsp->data, 0, data_size);
	}
	
	/* age out the past a little bit */
	for (size_t element = 0; element < row_count * column_count; element++)
		llsp->data[element] *= 1.0 - AGING_FACTOR;
	
	/* add new row to the top of the solving matrix */
	memmove(llsp->data + 1, llsp->data, data_size - sizeof(double));
	for (size_t column = 0; column < llsp->metrics; column++)
		llsp->full.matrix[column][0] = metrics[column];
	llsp->full.matrix[llsp->metrics][0] = target;
	
	/* givens fixup of the subdiagonal */
	for (size_t i = 0; i < llsp->sort.columns; i++)
		givens_fixup(llsp->sort, i + 1, i);
	
	llsp->last_measured = target;
}

const double *llsp_solve(llsp_t *restrict llsp)
{
	double *result = NULL;
	
	if (llsp->data) {
		stabilize(&llsp->sort, &llsp->good);
		trisolve(llsp->good);
		
		/* collect coefficients */
		size_t result_row = llsp->good.columns;
		for (size_t column = 0; column < llsp->metrics; column++)
			llsp->result[column] = llsp->full.matrix[column][result_row];
		result = llsp->result;
	}
	
	return result;
}

double llsp_predict(llsp_t *restrict llsp, const double *restrict metrics)
{
	/* calculate prediction by dot product */
	double result = 0.0;
	for (size_t i = 0; i < llsp->metrics; i++)
		result += llsp->result[i] * metrics[i];
	
	if (result >= EPSILON)
		return result;
	else
		return llsp->last_measured;
}

void llsp_dispose(llsp_t *restrict llsp)
{
	const size_t index_last = llsp->good.columns - 1;
	
	free(llsp->good.matrix[index_last]);
	free(llsp->full.matrix);
	free(llsp->sort.matrix);
	free(llsp->good.matrix);
	free(llsp->data);
	free(llsp);
}

#pragma mark -


#pragma mark Helper Functions

static void givens_fixup(struct matrix m, size_t row, size_t column)
{
	if (fabs(m.matrix[column][row]) < EPSILON) {  // alread zero
		m.matrix[column][row] = 0.0;  // reset to an actual zero for stability
		return;
	}
	
	const size_t i = row;
	const size_t j = column;
	const double a_ij = m.matrix[j][i];
	const double a_jj = m.matrix[j][j];
	const double rho = ((a_jj < 0.0) ? -1.0 : 1.0) * sqrt(a_jj * a_jj + a_ij * a_ij);
	const double c = a_jj / rho;
	const double s = a_ij / rho;
	
	for (size_t x = column; x < m.columns; x++) {
		if (x == column) {
			// the real calculation below should produce the same, but this is more stable
			m.matrix[x][i] = 0.0;
			m.matrix[x][j] = rho;
		} else {
			const double a_ix = m.matrix[x][i];
			const double a_jx = m.matrix[x][j];	
			m.matrix[x][i] = c * a_ix - s * a_jx;
			m.matrix[x][j] = s * a_ix + c * a_jx;
		}
		
		// reset to an actual zero for stability
		if (fabs(m.matrix[x][i]) < EPSILON)
			m.matrix[x][i] = 0.0;
		if (fabs(m.matrix[x][j]) < EPSILON)
			m.matrix[x][j] = 0.0;
	}
}

static void stabilize(struct matrix *sort, struct matrix *good)
{
	const size_t column_count = sort->columns;
	const size_t row_count = sort->columns + 1;  // extra row for shifting down and trisolve
	const size_t column_size = row_count * sizeof(double);
	const size_t index_last = column_count - 1;
	
	bool drop[column_count];
	double previous_residual = 0.0;
	
	good->columns = sort->columns;
	memcpy(good->matrix[index_last], sort->matrix[index_last], column_size);
	
	/* Drop columns from right to left and watch the residual error.
	 * We would actually copy the whole matrix, but when dropping from the right,
	 * Givens fixup always affects only the last column, so we hand just the
	 * last column through all possible positions. */
	for (size_t column = index_last; (ssize_t)column >= 0; column--) {
		good->matrix[column] = good->matrix[index_last];
		givens_fixup(*good, column + 1, column);
		
		double residual = fabs(good->matrix[column][column]);
		if (residual >= EPSILON && previous_residual >= EPSILON)
			drop[column] = (residual / previous_residual < COLUMN_CONTRIBUTION);
		else if (residual >= EPSILON && previous_residual < EPSILON)
			drop[column] = false;
		else
			drop[column] = true;
		
		previous_residual = residual;
		good->columns--;
	}
	/* The drop result for the last column is never used. The last column
	 * represents our target vector, so we must never drop it. */
	
	/* shuffle all to-be-dropped columns to the right */
	size_t keep_columns = index_last;  // number of columns to keep, starts with all
	for (size_t drop_column = index_last - 1; (ssize_t)drop_column >= 0; drop_column--) {
		if (!drop[drop_column]) continue;
		
		keep_columns--;
		
		if (drop_column < keep_columns) {  // column must move
			double *temp = sort->matrix[drop_column];
			memmove(&sort->matrix[drop_column], &sort->matrix[drop_column + 1],
					(keep_columns - drop_column) * sizeof(double *));
			sort->matrix[keep_columns] = temp;
			
			for (size_t column = drop_column; column < keep_columns; column++)
				givens_fixup(*sort, column + 1, column);
		}
	}
	
	/* setup good-column matrix */
	good->columns = sort->columns;
	memcpy(good->matrix, sort->matrix, keep_columns * sizeof(double *));  // non-drop columns
	memcpy(good->matrix[index_last], sort->matrix[index_last], column_size);   // copy last column
	
	/* Conceptually, we now drop the to-be-dropped columns from the right.
	 * Again, dropping the from the right only affects the residual error
	 * in the last column, so only it changes. Further, we no longer need
	 * the residual, so we can omit a proper Givens fixup and zero the
	 * residual instead.
	 * The resulting matrix has the same number of columns as the input,
	 * so the extra bottom-row used later by trisolve to store coefficients
	 * will be the actual bottom-row and not destroy triangularity.
	 * The resulting coeffients however will be the same as with an actual
	 * column-reduced matrix, because the diagonal elements for all
	 * dropped columns are zero. */
	for (size_t column = index_last; (ssize_t)column >= (ssize_t)keep_columns; column--) {
		good->matrix[column] = good->matrix[index_last];
		good->matrix[column][column] = 0.0;
	}
}

static void trisolve(struct matrix m)
{
	size_t result_row = m.columns;  // use extra row to solve the coefficients
	for (size_t column = 0; column < m.columns - 1; column++)
		m.matrix[column][result_row] = 0.0;
	
	for (size_t row = result_row - 2; (ssize_t)row >= 0; row--) {
		size_t column = row;
		
		if (fabs(m.matrix[column][row]) >= EPSILON) {
			column = m.columns - 1;
			
			double intermediate = m.matrix[column][row];
			for (column--; column > row; column--)
				intermediate -= m.matrix[column][result_row] * m.matrix[column][row];
			m.matrix[column][result_row] = intermediate / m.matrix[column][row];

			for (column--; (ssize_t)column >= 0; column--)
				// must be upper triangular matrix
				assert(m.matrix[column][row] == 0.0);
		} else
			m.matrix[column][row] = 0.0;  // reset to an actual zero for stability
	}
}
