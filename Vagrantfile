# -*- mode: ruby -*-
# vi: set ft=ruby :
Vagrant.configure("2") do |config|
  config.vm.box = "ubuntu/jammy64"
  config.vm.hostname = "kwok-test"

  config.vm.provider "virtualbox" do |vb|
    vb.memory = 8192
    vb.cpus   = 4
  end

  # Share your host ./bootstrap_vm into the guest
  config.vm.synced_folder "./bootstrap_vm", "/home/vagrant/bootstrap_vm", type: "virtualbox"

  # Call only 00_init.sh
  config.vm.provision "shell", inline: <<-SHELL
    set -e
    cd /home/vagrant/bootstrap_vm
    # Fix CRLF -> LF and make scripts executable (Windows hosts)
    sed -i 's/\\r$//' 00_init.sh 01_system_setup.sh 02_build_test.sh
    chmod +x 00_init.sh 01_system_setup.sh 02_build_test.sh
    # Run init with bash so 'pipefail' etc. work regardless of /bin/sh
    /usr/bin/env bash ./00_init.sh
  SHELL
end
