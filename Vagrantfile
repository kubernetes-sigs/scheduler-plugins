# -*- mode: ruby -*-
# vi: set ft=ruby :
Vagrant.configure("2") do |config|
  config.vm.box = "ubuntu/jammy64"
  config.vm.hostname = "kwok-test"

  config.vm.provider "virtualbox" do |vb|
    vb.memory = 8192
    vb.cpus   = 4
  end

  # Share bootstrap source from host (Windows) → guest
  config.vm.synced_folder "./bootstrap_vm", "/home/vagrant/bootstrap_vm", type: "virtualbox"

  config.vm.provision "shell", inline: <<-SHELL
    set -e
    cd /home/vagrant/bootstrap_vm

    # Convert CRLF -> LF for all scripts we’ll call
    sed -i 's/\\r$//' 00_init.sh 01_system_setup.sh 02_build_test.sh
    chmod +x 00_init.sh 01_system_setup.sh 02_build_test.sh

    # Run the single entrypoint with bash
    /usr/bin/env bash ./00_init.sh
  SHELL
end
