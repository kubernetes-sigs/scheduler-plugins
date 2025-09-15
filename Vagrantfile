Vagrant.configure("2") do |config|
  config.vm.box = "ubuntu/jammy64"
  config.vm.provider "virtualbox" do |vb|
    vb.memory = 8192
    vb.cpus   = 4
  end
  config.vm.hostname = "kwok-test"

  # sync only the bootstrap folder
  config.vm.synced_folder "./bootstrap", "/home/vagrant/bootstrap", type: "virtualbox"

  config.vm.provision "shell", inline: <<-'SHELL'
    set -e
    cd /home/vagrant/bootstrap
    /usr/bin/env bash ./bootstrap.sh all \
      --cluster kwok1 \
      --runtime binary \
      --config-dir "scripts/kwok/configs/baseline" \
      --results-dir "results" \
      --seed-file "scripts/kwok/seeds/seeds001.txt"
  SHELL
end
