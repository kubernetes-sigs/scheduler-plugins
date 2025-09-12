# -*- mode: ruby -*-
# vi: set ft=ruby :
Vagrant.configure("2") do |config|
  # Ubuntu 24.04 LTS
  config.vm.box = "ubuntu/jammy64"

  # Resources
  config.vm.provider "virtualbox" do |vb|
    vb.memory = 8192
    vb.cpus = 4
  end
  config.vm.hostname = "kwok-test"

  # Sync your bootstrap directory into the VM
  config.vm.synced_folder "./bootstrap_vm", "/home/vagrant/bootstrap_vm", type: "virtualbox"

  # Run scripts in order
  config.vm.provision "shell", inline: <<-SHELL
    set -euo pipefail
    cd /home/vagrant/bootstrap_vm

    # Make scripts executable
    chmod +x 01_system_setup.sh 02_build_test.sh

    # 1) Base system + docker (needs sudo/root)
    sudo ./01_system_setup.sh

    # 2) Run tests (normal user)
    ./02_build_test.sh
  SHELL
end