Vagrant.configure("2") do |config|
  config.vm.box = "ubuntu/jammy64"
  config.vm.provider "virtualbox" do |vb|
    vb.memory = 16384
    vb.cpus   = 8
  end
  config.vm.hostname = "kwok-test"

  # sync only the bootstrap folder
  config.vm.synced_folder "./bootstrap", "/home/vagrant/bootstrap", type: "virtualbox"

  # If --matrix-file is used, multiple test sessions can be run.
  # Parameters in matrix-file will overwrite other provided parameters (except --build-scheduler and --content-dir).
  config.vm.provision "shell", inline: <<-'SHELL'
    set -euo pipefail
    cd /home/vagrant/bootstrap
    find . -type f -name '*.sh' -print0 | xargs -0 -r sed -i 's/\r$//'
    /usr/bin/env bash ./bootstrap.sh all \
      --runner trace_replayer \
      --build-scheduler=false \
      --content-dir /home/vagrant/bootstrap/content \
      --cluster-name kwok1 \
      --kwok-runtime binary \
      --results-dir results/trace/test \
      --job-file data/jobs/trace/test/test_123.yaml \
      --solver-trigger true \
      --save-solver-stats true \
      --save-scheduler-logs true \
      --re-run-seeds true \
      --clean-start true \
      --monitor-interval 2
  SHELL
end

# NOTE: REMEMBER TO BUILD BEFORE VAGRANT UP, if not using prebuilt scheduler

# UCLOUD: --build-scheduler false --content-dir /work/<content-dir> --kwok-runtime binary --matrix-file data/jobs/2025-09-24/baseline_n4_p16_u090-01.csv