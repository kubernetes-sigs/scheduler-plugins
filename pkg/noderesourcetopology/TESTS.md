## NodeResourceTopologyMatch Tests

The NodeResourceTopologyMatch plugins involves with different Topology Manager configurations.

This differences leads to changing in the plugins behavior and needs to be tested in comprehensive way.

The most complicated and common case for these plugins is when `TopologyManagerPolicy=single-numa-node` and `TopologyManagerPolicy=container`.

Since this specific configuration has so many edge cases and permutations, we decided to summarize all of them in a table below. 
This will help us track the coverage and made sure the plugins are fully tested.

#### Test types:
- Each test type in the table below can be tested with a different type of resources. For example a test that check for overallocation, can be tested with CPUs overallocation or Memory overallocation or both.

### Table guidelines:
- All pods are `Guaranteed` QoS pods, IOW all containers are asking for an equal `request` and `limit` values.
- The table describes the coverage of the `TopologyManagerPolicy=single-numa-node` and `TopologyManagerScope=container` configuration. `TopologyManagerScope=container` is similar to `TopologyManagerScope=pod` but the key difference is the resource alignment scope which is that of a container (i.e. all resources must be allocated to the container from the same NUMA node) as opposed to a pod scope ( where resources of all containers within the pod must be aligned and hence allocated from the same NUMA node). For further reading please refer to  https://kubernetes.io/docs/tasks/administer-cluster/topology-manager/#topology-manager-scopes  


#### Table legend:
- R = Requested resources by the container
- NUMA = Resources available on a NUMA node.
- R [<,=,>] NUMA = Resources requested by container(s) are less/equal/more than the resources available on a NUMA node.
- R [<,=,>] Node = Resources requested by container(s) are less/equal/more than the resources available on the Node.
- SUM(R) = Sum of requested resources of all containers within a pod. 
- Node = Resources available under the whole node.
- N = Number of containers.
- NVM = What ever value in the field, it won't affect the test result (a placeholder for any possible value).

| test type | init containers | requests                         | containers | requests                         | state   |
|:----------|-----------------|----------------------------------|------------|----------------------------------|---------|
| 1         | 0               | N/A                              | 1          | R < NUMA                         | Running |
| 2         | 0               | N/A                              | 1          | NUMA < R < Node                  | Pending |
| 3         | 0               | N/A                              | N > 1      | SUM(R) < NUMA                    | Running |
| 4         | 0               | N/A                              | N > 1      | NUMA < SUM(R) < Node && R < NUMA | Running |
| 5         | 0               | N/A                              | N > 1      | SUM(R) > Node && R < NUMA        | Pending |
| 6         | 0               | N/A                              | N > 1      | NUMA < R < Node                  | Pending |
| 7         | 1               | NUMA < R < Node                  | NVM        | NVM                              | Pending |
| 8         | 1               | R < NUMA                         | 1          | R < NUMA                         | Running |
| 9         | 1               | R < NUMA                         | 1          | NUMA < R < Node                  | Pending |
| 10        | 1               | R < NUMA                         | N > 1      | SUM(R) < NUMA                    | Running |
| 11        | 1               | R < NUMA                         | N > 1      | NUMA < SUM(R) < Node && R < NUMA | Running |
| 12        | 1               | R < NUMA                         | N > 1      | SUM(R) > Node && R < NUMA        | Pending |
| 13        | 1               | R < NUMA                         | N > 1      | NUMA < R < Node                  | Pending |
| 14        | N > 1           | R < NUMA                         | 1          | R < NUMA                         | Running |
| 15        | N > 1           | R < NUMA                         | 1          | NUMA < R < Node                  | Pending |
| 16        | N > 1           | R < NUMA                         | N > 1      | SUM(R) < NUMA                    | Running |
| 17        | N > 1           | R < NUMA                         | N > 1      | NUMA < SUM(R) < Node && R < NUMA | Running |
| 18        | N > 1           | R < NUMA                         | N > 1      | SUM(R) > Node && R < NUMA        | Pending |
| 19        | N > 1           | R < NUMA                         | N > 1      | NUMA < R < Node                  | Pending |
| 20        | N > 1           | NUMA < SUM(R) < Node && R < NUMA | 1          | R < NUMA                         | Running |
| 21        | N > 1           | NUMA < SUM(R) < Node && R < NUMA | 1          | NUMA < R < Node                  | Pending |
| 22        | N > 1           | NUMA < SUM(R) < Node && R < NUMA | N > 1      | SUM(R) < NUMA                    | Pending |
| 23        | N > 1           | NUMA < SUM(R) < Node && R < NUMA | N > 1      | NUMA < SUM(R) < Node && R < NUMA | Running |
| 24        | N > 1           | NUMA < SUM(R) < Node && R < NUMA | N > 1      | SUM(R) > Node && R < NUMA        | Pending |
| 25        | N > 1           | NUMA < SUM(R) < Node && R < NUMA | N > 1      | NUMA < R < Node                  | Pending |
| 26        | N > 1           | SUM(R) > Node && R < NUMA        | 1          | R < NUMA                         | Running |
| 27        | N > 1           | SUM(R) > Node && R < NUMA        | 1          | NUMA < R < Node                  | Pending |
| 28        | N > 1           | SUM(R) > Node && R < NUMA        | N > 1      | SUM(R) < NUMA                    | Running |
| 29        | N > 1           | SUM(R) > Node && R < NUMA        | N > 1      | NUMA < SUM(R) < Node && R < NUMA | Running |
| 30        | N > 1           | SUM(R) > Node && R < NUMA        | N > 1      | SUM(R) > Node && R < NUMA        | Pending |
| 31        | N > 1           | SUM(R) > Node && R < NUMA        | N > 1      | NUMA < R < Node                  | Pending |
| 32        | N > 1           | NUMA < R < Node                  | NVM        | NVM                              | Pending |

### Test Tiers

#### Tier1
Contains the most fundamental and critical tests.
Init containers with multi/single apps containers placement against multiple NUMAs.
Making sure that plugin made the resources accounting correctly, and not mixing up init containers and standard containers resources together. 

#### Tier2
Multi/Single-containers (without init containers) placement against multiple NUMAs.
Basically should be a subset of Tier1 tests, but yet we need to make sure that without init containers the behavior remains as expected. 

#### Tier3 
Placement against single NUMA.
Checking that when all containers can be fit into the same NUMA, the behavior should be identical to pod scope. 
