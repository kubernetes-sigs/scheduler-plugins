# -*- mode: ruby -*-
# vi: set ft=ruby :
Vagrant.configure("2") do |config|
  config.vm.box = "ubuntu/jammy64"
  config.vm.provider "virtualbox" do |vb|
    vb.memory = 8192
    vb.cpus   = 4
  end
  config.vm.hostname = "kwok-test"

  # sync only the bootstrap folder
  config.vm.synced_folder "./bootstrap", "/home/vagrant/bootstrap", type: "virtualbox"

  # TODO: check how we can override using UCLOUD
  env = {
    "KWOK_CLUSTER" => "kwok1",
    "KWOK_CONFIGS" => "baseline",     # resolves to scripts/kwok/configs/baseline
    "KWOK_SEEDS"   => "seeds001.txt", # resolves to scripts/kwok/seeds/seeds001.txt
  }

  config.vm.provision "shell", env: env, inline: <<-'SHELL'
    set -e
    cd /home/vagrant/bootstrap
    /usr/bin/env bash ./00_init.sh
  SHELL
end
