output "vpc_id" {
  value = aws_vpc.this.id
}

output "vpc_cidr" {
  value = aws_vpc.this.cidr_block
}

output "public_subnet_ids" {
  value = aws_subnet.public[*].id
}

output "private_subnet_ids" {
  description = "EKS nodes and pods."
  value       = aws_subnet.private[*].id
}

output "data_subnet_ids" {
  description = "Aurora, MSK, ElastiCache, DocumentDB, OpenSearch. No route to the internet."
  value       = aws_subnet.data[*].id
}

output "availability_zones" {
  value = local.azs
}

output "nat_public_ips" {
  description = "Stable egress addresses. Payment providers and partner APIs allow-list these, so they belong in the runbook."
  value       = aws_eip.nat[*].public_ip
}

output "data_route_table_id" {
  value = aws_route_table.data.id
}
