module VagrantPlugins
  module CommandServe
    module Client
      class MachineIndex

        attr_reader :client
        attr_reader :broker

        def self.load(raw, broker:)
          conn = broker.dial(raw.stream_id)
          self.new(conn.to_s, broker)
        end

        def initialize(conn, broker=nil)
          @logger = Log4r::Logger.new("vagrant::command::serve::client::machineindex")
          @logger.debug("connecting to target index service on #{conn}")
          if !conn.nil?
            @client = SDK::TargetIndexService::Stub.new(conn, :this_channel_is_insecure)
          end
          @broker = broker
        end

        # @param [Hashicorp::Vagrant::Sdk::TargetIndex::TargetIdentifier]
        # @return [Boolean] true if delete is successful
        def delete(target)
          @logger.debug("deleting machine #{target} from index")
          @client.delete(target)
          true
        end

        # @param [Hashicorp::Vagrant::Sdk::TargetIndex::TargetIdentifier]
        # @return [Hashicorp::Vagrant::Sdk::Ref::Target]
        def get(ref)
          @logger.debug("getting machine with ref #{ref} from index")
          resp = @client.get(ref)
          return resp
        end

        # @param [Hashicorp::Vagrant::Sdk::TargetIndex::TargetIdentifier]
        # @return [Boolean]
        def include?(ref)
          @logger.debug("checking for machine with ref #{ref} in index")
          @client.includes(ref).exists
        end

        # @param [Hashicorp::Vagrant::Sdk::Args::Target] target
        # @return [Hashicorp::Vagrant::Sdk::Args::Target]  
        def set(target)
          @logger.debug("setting machine #{target} in index")
          @client.set(target)
        end

        # Get all targets
        # @return [Array<Hashicorp::Vagrant::Sdk::Args::Target>]  
        def all()
          @logger.debug("getting all machines")
          req = Google::Protobuf::Empty.new
          resp = @client.all(req)
          resp.targets
        end
      end
    end
  end
end
