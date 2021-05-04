<#
.SYNOPSIS 
    Installs Rancher RKE2 to create Windows Worker Nodes.
.DESCRIPTION 
    Run the script to install all Rancher RKE2 related neeeds. (kubernetes, docker, network) 
.NOTES
    Environment variables:
      System Agent Variables
      - CATTLE_AGENT_LOGLEVEL (default: debug)
      - CATTLE_AGENT_CONFIG_DIR (default: C:/etc/rancher/agent)
      - CATTLE_AGENT_VAR_DIR (default: C:/var/lib/rancher/agent)
      Rancher 2.6+ Variables
      - CATTLE_SERVER
      - CATTLE_TOKEN
      - CATTLE_CA_CHECKSUM
      - CATTLE_ROLE_CONTROLPLANE=false
      - CATTLE_ROLE_ETCD=false
      - CATTLE_ROLE_WORKER=false
      - CATTLE_LABELS
      - CATTLE_TAINTS
      Advanced Environment Variables
      - CATTLE_AGENT_BINARY_URL (default: latest GitHub release)
      - CATTLE_PRESERVE_WORKDIR (default: false)
      - CATTLE_REMOTE_ENABLED (default: true)
      - CATTLE_ID (default: autogenerate)
      - CATTLE_AGENT_BINARY_LOCAL (default: false)
      - CATTLE_AGENT_BINARY_LOCAL_LOCATION (default: )
.EXAMPLE 
    
#>
[CmdletBinding()]
param (
    [Parameter()]
    [Switch]
    $Worker,
    [Parameter()]
    [String]
    $Label,
    [Parameter()]
    [String]
    $Taint,
    [Parameter()]
    [String]
    $Token
)

function Write-LogInfo {
    Write-Host -NoNewline -ForegroundColor Blue "INFO: "
    Write-Host -ForegroundColor Gray ("{0,-44}" -f ($args -join " "))
}
function Write-LogWarn {
    Write-Host -NoNewline -ForegroundColor DarkYellow "WARN: "
    Write-Host -ForegroundColor Gray ("{0,-44}" -f ($args -join " "))
}
function Write-LogError {
    Write-Host -NoNewline -ForegroundColor DarkRed "ERRO: "
    Write-Host -ForegroundColor Gray ("{0,-44}" -f ($args -join " "))
}
function Write-LogFatal {
    Write-Host -NoNewline -ForegroundColor DarkRed "FATA: "
    Write-Host -ForegroundColor Gray ("{0,-44}" -f ($args -join " "))
    exit 255
}

function Get-Args {
    if ($Worker) {
        $env:CATTLE_ROLE_WORKER = "true"
    }
    
    if ($Label) {
        if ($env:CATTLE_LABELS) {
            $env:CATTLE_LABELS += $Label
        }
        else {
            $env:CATTLE_LABELS = $Label
        }
    }
                    
    if ($Taint) {
        if ($env:CATTLE_TAINTS) {
            $env:CATTLE_TAINTS += $Taint
        }
        else {
            $env:CATTLE_TAINTS = $Taint
        }
    }
    
    if ($Token) {
        $env:CATTLE_TOKEN = $Token
    }
}

function Set-Environment {
    if (-Not $env:CATTLE_ROLE_CONTROLPLANE) {
        $env:CATTLE_ROLE_CONTROLPLANE = "false"
    } 

    if (-Not $env:CATTLE_ROLE_ETCD) {
        $env:CATTLE_ROLE_ETCD = "false"
    } 

    if (-Not $env:CATTLE_ROLE_WORKER) {
        $env:CATTLE_ROLE_WORKER = "false"
    } 

    if (-Not $env:CATTLE_REMOTE_ENABLED) {
        $env:CATTLE_REMOTE_ENABLED = "true"
    } 
    else {
        $env:CATTLE_REMOTE_ENABLED = "$(echo "${CATTLE_REMOTE_ENABLED}" | tr '[:upper:]' '[:lower:]')"
    }

    if (-Not $env:CATTLE_PRESERVE_WORKDIR) {
        $env:CATTLE_PRESERVE_WORKDIR = "false"
    } 
    else {
        $env:CATTLE_PRESERVE_WORKDIR = "$(echo "${CATTLE_PRESERVE_WORKDIR}" | tr '[:upper:]' '[:lower:]')"
    }

    if (-Not $env:CATTLE_AGENT_LOGLEVEL) {
        $env:CATTLE_AGENT_LOGLEVEL = "debug"
    } 
    else {
        $env:CATTLE_AGENT_LOGLEVEL = "$(echo "${CATTLE_AGENT_LOGLEVEL}" | tr '[:upper:]' '[:lower:]')"
    }

    if ($env:CATTLE_AGENT_BINARY_LOCAL -eq "true") {
        if (-Not $env:CATTLE_AGENT_BINARY_LOCAL_LOCATION) {
            Write-LogFatal "No local binary location was specified"
        }
    }
    else {
        if (-Not $env:CATTLE_AGENT_BINARY_URL) {
            $fallback = "v0.0.1-alpha1"
            $rateInfo = Invoke-RestMethod -Uri https://api.github.com/rate_limit 
            if ($rateInfo.rate.remaining -eq 0) {
                Write-LogInfo "GitHub Rate Limit exceeded, falling back to known good version"
                $env:VERSION = $fallback
            }
            else {
                $versionInfo = Invoke-RestMethod -Uri https://api.github.com/repos/rancher/system-agent/releases/latest
                $env:VERSION = $versionInfo.tag_name
                if (-Not $env:VERSION) {
                    Write-LogError "Error contacting GitHub to retrieve the latest version"
                    $env:VERSION = $fallback
                }
            }
        }
        # TODO: This isn't correct at all, needs windows touch to it.
        $env:CATTLE_AGENT_BINARY_URL = "https://github.com/rancher/system-agent/releases/download/$($env:VERSION)/rancher-system-agent_windows-$(($env:PROCESSOR_ARCHITECTURE).ToLower()).exe"
    }

    if ($env:CATTLE_REMOTE_ENABLED -eq "true") {
        if (-Not $env:CATTLE_TOKEN) {
            Write-LogInfo "Environment variable CATTLE_TOKEN was not set. Will not retrieve a remote connection configuration from Rancher2"
        }
        else {
            if (-Not $env:CATTLE_SERVER) {
                Write-LogFatal "Environment variable CATTLE_SERVER was not set"
            }
        }
    } 

    if (-Not $env:CATTLE_AGENT_CONFIG_DIR) {
        $env:CATTLE_AGENT_CONFIG_DIR = "C:/etc/rancher/agent"
        Write-LogInfo "Using default agent configuration directory $($env:CATTLE_AGENT_CONFIG_DIR)"
    }

    if (-Not $env:CATTLE_AGENT_VAR_DIR) {
        $env:CATTLE_AGENT_VAR_DIR = "C:/etc/rancher/agent"
        Write-LogInfo "Using default agent var directory $($env:CATTLE_AGENT_VAR_DIR)"
    }
}

function New-Directories() {
    New-Item -ItemType Directory -Path $env:CATTLE_AGENT_VAR_DIR
    New-Item -ItemType Directory -Path $env:CATTLE_AGENT_CONFIG_DIR
}

function Test-Architecture() {
    if ($env:PROCESSOR_ARCHITECTURE -ne "AMD64") {
        Write-LogFatal "Unsupported architecture $($env:PROCESSOR_ARCHITECTUR)"
    }
} 

function Invoke-RancherAgentDownload() {
    $localLocation = "C:/Program Files/Rancher/rancher-system-agent.exe"
    if ($env:CATTLE_AGENT_BINARY_LOCAL) {
        Write-LogInfo "Using local rancher-system-agent binary from $($env:CATTLE_AGENT_BINARY_LOCAL_LOCATION)"
        Copy-Item -Path $env:CATTLE_AGENT_BINARY_LOCAL -Destination $localLocation
    }
    else {
        Write-LogInfo "Downloading rancher-system-agent from $($env:CATTLE_AGENT_BINARY_URL)"
        Invoke-RestMethod -Uri $env:CATTLE_AGENT_BINARY_URL -OutFile $localLocation
    }
}

function Test-X509Cert

function Test-CaCheckSum() {
    $caCertsPath = "cacerts"
    $tempPath = "$env:TEMP/ranchercert"
    if(-Not $env:CATTLE_CA_CHECKSUM) {
        Invoke-RestMethod -Url $env:CATTLE_SERVER/$caCertsPath -OutFile $tempPath -SkipCertificateCheck
        if(-Not (Test-Path -Path tempPath)) {
            Write-Error "The environment variable CATTLE_CA_CHECKSUM is set but there is no CA certificate configured at $(env:CATTLE_SERVER/$caCertsPath)) "
            exit 1
        }
        openssl x509 -in $cert -noout
        if($LASTEXITCODE -ne 0) {
            Write-Error "Value from $($env:CATTLE_SERVER)/$($caCertsPath) does not look like an x509 certificate, exited with $($LASTEXITCODE) "
            Write-Error "Retrieved cacerts:"
            Get-Content $tempPath
            exit 1
        }
        else {
            info "Value from $($env:CATTLE_SERVER)/$($caCertsPath) is an x509 certificate"
        }
        $env:CATTLE_SERVER_CHECKSUM = (Get-FileHash -Path $tempPath -Algorithm SHA256).Hash.ToLower()
        if($env:CATTLE_SERVER_CHECKSUM -ne $env:CATTLE_CA_CHECKSUM) {
            Remove-Item -Path $tempPath -Force
            Write-LogError "Configured cacerts checksum $($env:CATTLE_SERVER_CHECKSUM) does not match given --ca-checksum $($env:CATTLE_CA_CHECKSUM) "
            Write-LogError "Please check if the correct certificate is configured at $($env:CATTLE_SERVER)/$($caCertsPath) ."
            exit 1
        }
    }
}

function Get-ConnectionInfo() {
    if ($env:CATTLE_REMOTE_ENABLED = "true") {
        $requestParams = @{
            Uri     = "$($env:CATTLE_SERVER)/v3/connect/agent"
            Headers = @{
                'Authorization'               = "Bearer $($env:CATTLE_TOKEN)"
                'X-Cattle-Id'                 = $env:CATTLE_ID
                'X-Cattle-Role-Etcd'          = $env:CATTLE_ROLE_ETCD
                'X-Cattle-Role-Control-Plane' = $env:CATTLE_ROLE_CONTROLPLANE
                'X-Cattle-Role-Worker'        = $env:CATTLE_ROLE_WORKER
                'X-Cattle-Labels'             = $env:CATTLE_LABELS
                'X-Cattle-Taints'             = $env:CATTLE_TAINTS
            }
            OutFile = "$($env:CATTLE_AGENT_VAR_DIR)/rancher2_connection_info.json"
        }

        if (-Not $env:CATTLE_CA_CHECKSUM) {
            Invoke-RestMethod @requestParams
        }
        else {
            Invoke-RestMethod @requestParams -SkipCertificateCheck
        }
    }
}

function Set-RancherConfig() {
    $config = "workDirectory: $($env:CATTLE_AGENT_VAR_DIR)/work
localPlanDirectory: $($env:CATTLE_AGENT_VAR_DIR)/plans
appliedPlanDirectory: $($env:CATTLE_AGENT_VAR_DIR)/applied
remoteEnabled:$($env:CATTLE_REMOTE_ENABLED)
preserveWorkDirectory: $($env:CATTLE_PRESERVE_WORKDIR)
    "

    if ($env:CATTLE_REMOTE_ENABLED) {
        $config += "`n connectionInfoFile: $($env:CATTLE_AGENT_VAR_DIR)/rancher2_connection_info.json"
    }

    Set-Content -Path $env:CATTLE_AGENT_CONFIG_DIR/config.yaml -Value $config
}

function New-CattleId() {
    if (-Not $env:CATTLE_ID) {
        Write-LogInfo "Generating Cattle ID"

        if (Test-Path -Path "$($env:CATTLE_AGENT_CONFIG_DIR)/cattle-id") {
            $env:CATTLE_ID = Get-Content -Path "$($env:CATTLE_AGENT_CONFIG_DIR)/cattle-id"
            Write-LogInfo "Cattle ID was already detected as $($env:CATTLE_ID). Not generating a new one."
            return
        }
        $stream = [IO.MemoryStream]::new([Text.Encoding]::UTF8.GetBytes($env:COMPUTERNAME))
        $env:CATTLE_ID = (Get-FileHash -InputStream $stream -Algorithm SHA256).Hash.ToLower()
        Set-Content -Path "$($env:CATTLE_AGENT_CONFIG_DIR)/cattle-id" -Value $env:CATTLE_ID
        return
    }
    Write-LogInfo "Not generating Cattle ID"
}

function Invoke-RancherInstall () {
    $rancherServiceName = "rancher-system-agent"
    Get-Args
    Set-Environment
    New-Directories
    
    if (-Not $env:CATTLE_CA_CHECKSUM) { Test-CaCheckSum }

    if ((Get-Service -Name $rancherServiceName -ErrorAction SilentlyContinue)) {
        Stop-Service -Name $rancherServiceName
        while ((Get-Service $rancherServiceName).Status -ne 'Stopped') { Start-Sleep -s 5 }
    }

    Invoke-RancherAgentDownload
    Set-RancherConfig

    if (-Not $env:CATTLE_TOKEN) {
        New-CattleId
        Get-ConnectionInfo
    }

    # Create Windows Service
    Write-LogInfo "Enabling rancher-system-agent service"
    if ((Get-Service -Name $rancherServiceName -ErrorAction SilentlyContinue)) {
        Write-LogInfo "Starting/restarting rancher-system-agent service"
        Start-Service -Name $rancherServiceName
    }
    else {
        Write-LogInfo "Creating and starting rancher-system-agent service"
        $serviceParams = @{
            Name           = "rancher-system-agent"
            BinaryPathName = '"C:\Program FilesRancher\rancher-system-agent.exe"'
            DisplayName    = "Rancher System Agent"
            StartupType    = "AutomaticDelayedStart "
            Description    = "This the Rancher System Agent for running RKE2."
        }
        New-Service @serviceParams
        Start-Service -Name $rancherServiceName
    }
}

Invoke-RancherInstall
exit 0