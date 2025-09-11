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
    chmod +x 01_system_setup.sh 02_go.sh 03_tools.sh 04_clone_and_build.sh 05_test.sh

    # 1) Base system + docker (needs sudo/root)
    sudo ./01_system_setup.sh

    # 2) go
    sudo ./02_go.sh

    # 2) kubectl + kwokctl (sudo/root)
    sudo ./03_tools.sh

    # 3) Clone repo + build & push image (normal user)
    ./04_clone_and_build.sh

    # 4) Run tests (normal user)
    ./05_test.sh
  SHELL
end
