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
  config.vm.synced_folder "./bootstrap", "/home/vagrant/bootstrap", type: "virtualbox"

  config.vm.provision "shell", inline: <<-SHELL
    set -e
    cd /home/vagrant/bootstrap

    for f in 00_init.sh 01_system_setup.sh 02_build_test.sh; do
      # strip UTF-8 BOM if present
      sed -i '1s/^\xef\xbb\xbf//' "$f"
      # convert CRLF -> LF
      sed -i 's/\r$//' "$f"
      chmod +x "$f"
    done

    # always run with bash
    /usr/bin/env bash ./00_init.sh
  SHELL
end
