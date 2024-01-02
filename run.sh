#!/bin/bash -e
VPC_ID=""
CLUSTER_NAME=""
AWS_REGION="eu-west-1"
POSITIONAL_ARGS=()
export AWS_REGION

number_of_args="$#"
while (( number_of_args > 0 )); do
  case "$1" in
    "--vpc-id")
      VPC_ID="$2"
      shift
      shift
      ;;
    "--cluster-name")
      CLUSTER_NAME="$2"
      shift
      shift
      ;;
    "--region")
      AWS_REGION="$2"
      export AWS_REGION
      shift
      shift
      ;;
    *)
      POSITIONAL_ARGS+=("$1")
      shift
  esac
  number_of_args="$#"
done

set -- "${POSITIONAL_ARGS[@]}" # restore positional parameters

function delete_stack() {
  echo "Deleting stack $1"
  aws cloudformation delete-stack --stack-name "$1"
  while aws cloudformation describe-stacks --stack-name "$1" &> /dev/null; do
    echo "Stack $1 still exists. Waiting"
    sleep 20
  done
  echo "Stack $1 deleted"
}

function get_oidc_id() {
    oidc_id=$(aws eks describe-cluster \
      --name "${CLUSTER_NAME}" \
      --query "cluster.identity.oidc.issuer" \
      --output text | cut -d '/' -f 5)
}

function install() {
  if [ "${CLUSTER_NAME}" = "" ]; then
    echo "Cluster Name should be specified"
    exit 255
  fi
  pushd infra
  get_oidc_id
  local namespace="system"
  local service_account_name="controller-manager"
  aws cloudformation deploy \
    --stack-name "${CLUSTER_NAME}-pomidor-iam" \
    --capabilities "CAPABILITY_IAM" "CAPABILITY_NAMED_IAM" \
    --template-file iam.yaml \
    --parameter-overrides "Namespace=${namespace}" "ServiceAccount=${service_account_name}" \
    "OidcId=${oidc_id}" "ClusterName=${CLUSTER_NAME}"
  aws cloudformation deploy \
    --stack-name "${CLUSTER_NAME}-pomidor-ecr" \
    --template-file ecr.yaml \
    --parameter-overrides "ClusterName=${CLUSTER_NAME}"
  popd
}

function uninstall() {
  if [ "${CLUSTER_NAME}" = "" ]; then
    echo "Cluster Name should be specified"
    exit 255
  fi
  pushd infra
  delete_stack "${CLUSTER_NAME}-pomidor-iam"
  delete_stack "${CLUSTER_NAME}-pomidor-ecr"
  popd
}

case "$1" in
  "install") install ;;
  "uninstall") uninstall ;;
esac